package openrouter

import (
	"log/slog"
	"net/http"
	"time"
)

// logTransport is the package's http.RoundTripper. It wraps an
// underlying RoundTripper and emits a single structured log record per
// HTTP attempt with {model, duration_ms, status} plus an optional
// {tokens_in, tokens_out} pair fed back by the Client once the
// response body has been decoded.
//
// What it MUST NOT log:
//
//   - Request body (contains the user prompt).
//   - Response body (contains the LLM completion).
//   - Authorization header (Bearer API key).
//   - X-Idempotency-Key header (internal tenant/conversation/request
//     correlation; safe in principle but irrelevant in transit logs).
//
// The Authorization redaction is what made W3A worth a dedicated
// RoundTripper instead of inline log.Printf — keeping the secret out
// of any future "log the whole request" debug feature is a
// defence-in-depth property of the package, per decision #8 / memory
// feedback_run_gofmt_before_commit_crm.md.
type logTransport struct {
	base   http.RoundTripper
	logger *slog.Logger
	now    func() time.Time
}

// newLogTransport returns a RoundTripper that logs each HTTP attempt
// via the given slog.Logger. A nil base falls back to
// http.DefaultTransport so callers can simply pass the default client's
// transport when none is configured. A nil logger falls back to
// slog.Default; the package-level Client construction guarantees a
// non-nil logger but defensive defaults keep the type useful in tests.
func newLogTransport(base http.RoundTripper, logger *slog.Logger) *logTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &logTransport{base: base, logger: logger, now: time.Now}
}

// RoundTrip implements http.RoundTripper. The model label is extracted
// from a request-scoped attribute set by Client.Complete before each
// attempt — we deliberately do NOT inspect the JSON body to find the
// model name, because that would tempt future maintainers into logging
// other body fields.
func (t *logTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	model := modelFromRequest(req)
	start := t.now()
	resp, err := t.base.RoundTrip(req)
	durMs := t.now().Sub(start).Milliseconds()

	if err != nil {
		t.logger.LogAttrs(req.Context(), slog.LevelWarn, "openrouter request failed",
			slog.String("model", model),
			slog.Int64("duration_ms", durMs),
			slog.String("error", err.Error()),
		)
		return nil, err
	}

	t.logger.LogAttrs(req.Context(), slog.LevelInfo, "openrouter request",
		slog.String("model", model),
		slog.Int64("duration_ms", durMs),
		slog.Int("status", resp.StatusCode),
	)
	return resp, nil
}

// requestModelCtxKey is the context-key family used by Client.Complete
// to thread the model label through to the RoundTripper without
// inspecting the body. Unexported so external packages cannot inject a
// fake model into the log line.
type requestModelCtxKey struct{}

// modelFromRequest extracts the model label set by withModel. Returns
// "unknown" so logs never carry an empty label and Prometheus aggregation
// stays clean.
func modelFromRequest(req *http.Request) string {
	if req == nil || req.Context() == nil {
		return "unknown"
	}
	if v, ok := req.Context().Value(requestModelCtxKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}
