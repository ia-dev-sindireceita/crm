package obs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/pericles-luz/crm/internal/obs"
)

// withRecorder swaps the global TracerProvider for a recording one
// that captures every span end. Restored on test cleanup.
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return rec
}

func TestNewTracerProvider_NoEndpoint_ReturnsNoop(t *testing.T) {
	// Cannot run in parallel — mutates global tracer provider.
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tp, shutdown, err := obs.NewTracerProvider(context.Background(), "crm-test", "")
	if err != nil {
		t.Fatalf("NewTracerProvider(empty): %v", err)
	}
	if tp == nil {
		t.Fatal("NewTracerProvider returned nil provider")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned %v", err)
	}
}

func TestNewTracerProvider_HTTPEndpoint_Works(t *testing.T) {
	// Cannot run in parallel.
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	endpoint := stripScheme(srv.URL)
	tp, shutdown, err := obs.NewTracerProvider(context.Background(), "crm-test", endpoint)
	if err != nil {
		t.Fatalf("NewTracerProvider(real): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "unit")
	span.End()

	// Successful shutdown is the assertion: it must flush without
	// timing out against our fake collector.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned %v", err)
	}
}

// stripScheme converts http://127.0.0.1:9999 → 127.0.0.1:9999 because
// otlptracehttp.WithEndpoint expects host:port (it adds its own scheme).
func stripScheme(u string) string {
	for i := 0; i < len(u)-2; i++ {
		if u[i] == ':' && u[i+1] == '/' && u[i+2] == '/' {
			return u[i+3:]
		}
	}
	return u
}

func TestTracer_NotNil(t *testing.T) {
	t.Parallel()
	if obs.Tracer("x") == nil {
		t.Fatal("Tracer returned nil")
	}
}

func TestOTelHTTP_StartsServerSpanWithRouteAttr(t *testing.T) {
	rec := withRecorder(t)
	mw := obs.OTelHTTP("http.request",
		func(*http.Request) string { return "/login" },
	)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	rr := httptest.NewRecorder()
	final.ServeHTTP(rr, req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	got := spans[0]
	if got.SpanKind() != oteltrace.SpanKindServer {
		t.Errorf("span kind: got %v, want Server", got.SpanKind())
	}
	if got.Name() != "POST /login" {
		t.Errorf("span name: got %q", got.Name())
	}
	hasRoute := false
	for _, attr := range got.Attributes() {
		if attr.Key == "route" && attr.Value.AsString() == "/login" {
			hasRoute = true
		}
	}
	if !hasRoute {
		t.Errorf("route attribute missing: %v", got.Attributes())
	}
}

func TestOTelHTTP_AppliesEnricher(t *testing.T) {
	rec := withRecorder(t)
	mw := obs.OTelHTTP(
		"http.request",
		func(*http.Request) string { return "/x" },
		func(*http.Request) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("tenant.id", "t-1")}
		},
		nil, // skipped silently
		func(*http.Request) []attribute.KeyValue { return nil },
	)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	final.ServeHTTP(rr, req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	hasTenant := false
	for _, attr := range spans[0].Attributes() {
		if attr.Key == "tenant.id" && attr.Value.AsString() == "t-1" {
			hasTenant = true
		}
	}
	if !hasTenant {
		t.Errorf("tenant.id attr missing: %v", spans[0].Attributes())
	}
}

func TestOTelHTTP_EmptyArgsUseDefaults(t *testing.T) {
	rec := withRecorder(t)
	mw := obs.OTelHTTP("", nil) // both fall back to defaults
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/default-path", nil)
	rr := httptest.NewRecorder()
	final.ServeHTTP(rr, req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Name() != "GET /default-path" {
		t.Errorf("default route fallback failed: %q", spans[0].Name())
	}
}
