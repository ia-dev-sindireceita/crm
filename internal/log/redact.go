// Package log wraps slog with a redacting handler that scrubs sensitive
// field values before they reach any log sink. It is the single logging
// seam for the CRM application (ADR 0004).
//
// Redaction applies to two orthogonal signal sources:
//
//  1. Key-name allowlist (case-insensitive): any slog attribute whose
//     key matches a known PII/secret name is replaced with "[REDACTED]".
//  2. Struct tag `log:"redact"`: when a struct value is passed as an
//     slog.Attr, any exported field carrying the tag is zeroed before
//     the value is serialised.
//
// Raw payloads (arbitrary []byte / string) MUST NOT be logged at level
// Info or below. Use LogRawEvent at level Error, which hashes the
// payload and logs only the SHA-256 digest and a stable UUID instead.
package log

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const redactedValue = "[REDACTED]"

// sensitiveKeys is the case-insensitive allowlist of attribute keys whose
// values must never appear in log output.
var sensitiveKeys = func() map[string]struct{} {
	keys := []string{
		"password", "passwd", "pwd",
		"token", "jwt", "refresh_token", "access_token",
		"api_key", "authorization",
		"cookie", "set_cookie",
		"secret", "recovery_code", "totp_secret", "otp_seed",
		"cpf", "cnpj",
		"phone", "telefone",
		"email", "e_mail", "mail",
	}
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}()

func isSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[strings.ToLower(key)]
	return ok
}

// redactHandler wraps an inner slog.Handler and scrubs sensitive attrs.
type redactHandler struct{ inner slog.Handler }

// New returns a *slog.Logger whose handler scrubs sensitive attributes.
func New(w io.Writer, level slog.Level) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(&redactHandler{inner: base})
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	cleaned := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		cleaned.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, cleaned)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(scrubbed)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

// scrubAttr replaces the value of a sensitive key with redactedValue and
// recursively scrubs group attributes. Struct values are scrub-copied via
// reflection so that fields tagged `log:"redact"` are zeroed.
func scrubAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()

	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedValue)
	}

	switch a.Value.Kind() {
	case slog.KindGroup:
		sub := a.Value.Group()
		cleaned := make([]slog.Attr, len(sub))
		for i, s := range sub {
			cleaned[i] = scrubAttr(s)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(cleaned...)}

	case slog.KindAny:
		if v := scrubStructValue(a.Value.Any()); v != nil {
			return slog.Any(a.Key, v)
		}
	}

	return a
}

// --- reflection-based struct scrubbing ---

// typeCache memoises the redacted field indices for each struct type.
var typeCache sync.Map // reflect.Type → []int (field indices with log:"redact")

func redactedFields(t reflect.Type) []int {
	if v, ok := typeCache.Load(t); ok {
		return v.([]int)
	}
	var indices []int
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("log") == "redact" {
			indices = append(indices, i)
		}
	}
	typeCache.Store(t, indices)
	return indices
}

// scrubStructValue returns a pointer to a copy of v (which must be a
// struct or *struct) with log:"redact" fields zeroed. Returns nil if v
// is not a struct type, allowing the caller to fall through.
func scrubStructValue(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}

	indices := redactedFields(rv.Type())
	if len(indices) == 0 {
		return nil // no tagged fields — no copy needed
	}

	// shallow copy so we don't mutate the caller's value
	cp := reflect.New(rv.Type()).Elem()
	cp.Set(rv)
	for _, i := range indices {
		f := cp.Field(i)
		if f.CanSet() {
			f.Set(reflect.Zero(f.Type()))
		}
	}
	return cp.Interface()
}

// --- raw-event helper ---

// RawEvent carries the opaque identifier and content hash that are safe
// to log at Error level instead of raw payload bytes.
type RawEvent struct {
	ID         string // stable UUID for correlation
	PayloadSHA string // hex-encoded SHA-256 of the raw payload
}

// NewRawEvent hashes payload and returns a RawEvent. The payload itself
// is never retained.
func NewRawEvent(payload []byte) RawEvent {
	sum := sha256.Sum256(payload)
	return RawEvent{
		ID:         uuid.New().String(),
		PayloadSHA: fmt.Sprintf("%x", sum[:]),
	}
}

// LogRawEvent logs a RawEvent at Error level. Callers MUST NOT include
// the raw payload in any other log line at Info or below; use this
// function exclusively for raw-payload audit trails.
func LogRawEvent(ctx context.Context, logger *slog.Logger, msg string, ev RawEvent, extra ...any) {
	args := append([]any{
		"raw_event_id", ev.ID,
		"payload_sha256", ev.PayloadSHA,
	}, extra...)
	logger.ErrorContext(ctx, msg, args...)
}
