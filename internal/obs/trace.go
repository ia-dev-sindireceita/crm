package obs

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerProviderShutdown is the cleanup callback returned by
// NewTracerProvider. Callers MUST defer it during graceful shutdown
// so spans flush; the SDK's batch processor drops in-flight spans
// otherwise.
type TracerProviderShutdown func(context.Context) error

// NewTracerProvider returns a configured trace.TracerProvider backed
// by an OTLP HTTP exporter targeting endpoint, plus a shutdown
// callback. When endpoint is empty the function returns a no-op
// provider — useful in tests and in dev rigs without a collector.
//
// serviceName is required: it ends up on every span as the standard
// service.name resource attribute.
func NewTracerProvider(ctx context.Context, serviceName, endpoint string) (trace.TracerProvider, TracerProviderShutdown, error) {
	if endpoint == "" {
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return tp, func(context.Context) error { return nil }, nil
	}

	exp, err := otlptrace.New(ctx, otlptracehttp.NewClient(
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	))
	if err != nil {
		return nil, nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp, func(shutdownCtx context.Context) error {
		// Bound the shutdown so a wedged collector cannot block
		// process exit. 5s is enough to flush a quiet steady state.
		c, cancel := context.WithTimeout(shutdownCtx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(c)
	}, nil
}

// Tracer returns a named tracer from the global provider. Domain code
// goes through this — never reach for otel.Tracer directly so we keep
// a single observable seam.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Enricher computes additional span attributes from a request's
// context. The OTelHTTP middleware applies all enrichers in order;
// returning a nil/empty slice is a no-op.
type Enricher func(*http.Request) []attribute.KeyValue

// OTelHTTP wraps next with a server-kind span named "method route"
// (or "method path" when the chi RouteContext is unavailable). It is
// deliberately decoupled from the tenancy/iam packages: callers pass
// Enricher closures to attach tenant.id and user.id without obs
// importing those types.
func OTelHTTP(spanName string, route func(*http.Request) string, enrichers ...Enricher) func(http.Handler) http.Handler {
	if spanName == "" {
		spanName = "http.request"
	}
	if route == nil {
		route = func(r *http.Request) string { return r.URL.Path }
	}
	tracer := otel.Tracer("github.com/pericles-luz/crm/internal/obs")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rt := route(r)
			ctx, span := tracer.Start(r.Context(), r.Method+" "+rt,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("route", rt),
					attribute.String("http.target", r.URL.Path),
				),
			)
			defer span.End()
			for _, e := range enrichers {
				if e == nil {
					continue
				}
				if attrs := e(r); len(attrs) > 0 {
					span.SetAttributes(attrs...)
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
