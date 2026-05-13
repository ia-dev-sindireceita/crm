// Package obs is the observability seam for SIN-62218: structured JSON
// logging on stdout (slog), OTLP traces, and a Prometheus metrics
// registry. The package is intentionally tiny — its only job is to be
// importable from anywhere (handlers, adapters, domain) without
// dragging HTTP/DB/IAM types along.
//
// Naming follows the OpenTelemetry semantic conventions where it is
// cheap to: tenant.id / user.id on spans, but slog attributes use the
// flat tenant_id / request_id / user_id snake_case for jq friendliness.
//
// PII discipline: NEVER log raw email, password, or message body.
// Loggers emit only ids and counts. Field redaction lives at the
// callsite — this package does not try to scrub arbitrary attributes.
package obs

import (
	"context"
	"io"
	"log/slog"
)

// ctxKeyType is the unexported key family used to enrich slog logs. We
// avoid string keys to dodge collisions with other packages.
type ctxKeyType int

const (
	ctxKeyTenantID ctxKeyType = iota + 1
	ctxKeyRequestID
	ctxKeyUserID
)

// WithTenantID, WithRequestID, WithUserID seed context values that
// FromContext later promotes to slog attributes. Callers (HTTP
// middleware) own the lifecycle.
func WithTenantID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyTenantID, id)
}

func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func WithUserID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyUserID, id)
}

func ctxString(ctx context.Context, k ctxKeyType) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(k).(string)
	return v
}

// NewJSONLogger returns a *slog.Logger that writes JSON to w. The
// returned logger uses a context-aware handler so FromContext-style
// enrichment works without re-allocating the logger per request.
func NewJSONLogger(w io.Writer, level slog.Level) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(&ctxHandler{Handler: base})
}

// ctxHandler wraps an underlying slog.Handler and lifts tenant_id /
// request_id / user_id from the context onto every record. Empty
// values are dropped so log output stays compact.
type ctxHandler struct{ slog.Handler }

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if v := ctxString(ctx, ctxKeyTenantID); v != "" {
		r.AddAttrs(slog.String("tenant_id", v))
	}
	if v := ctxString(ctx, ctxKeyRequestID); v != "" {
		r.AddAttrs(slog.String("request_id", v))
	}
	if v := ctxString(ctx, ctxKeyUserID); v != "" {
		r.AddAttrs(slog.String("user_id", v))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithGroup(name)}
}

// FromContext returns a logger pre-bound to the tenant_id /
// request_id / user_id present on ctx. The returned logger is a thin
// wrapper around slog.Default(); call sites that need a project-wide
// logger should set slog.SetDefault at bootstrap.
//
// FromContext never returns nil. A bare slog.Default() is returned
// when ctx carries no enrichment values.
func FromContext(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if ctx == nil {
		return l
	}
	attrs := make([]slog.Attr, 0, 3)
	if v := ctxString(ctx, ctxKeyTenantID); v != "" {
		attrs = append(attrs, slog.String("tenant_id", v))
	}
	if v := ctxString(ctx, ctxKeyRequestID); v != "" {
		attrs = append(attrs, slog.String("request_id", v))
	}
	if v := ctxString(ctx, ctxKeyUserID); v != "" {
		attrs = append(attrs, slog.String("user_id", v))
	}
	if len(attrs) == 0 {
		return l
	}
	return slog.New(l.Handler().WithAttrs(attrs))
}

// RedactedEmail collapses an email to its first letter + "@…domain"
// for logs. Bare emails MUST NOT be logged anywhere; callers reach
// for this helper at the boundary.
func RedactedEmail(email string) string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			if i == 0 {
				return "[redacted]@" + email[i+1:]
			}
			return string(email[0]) + "***@" + email[i+1:]
		}
	}
	return "[redacted]"
}
