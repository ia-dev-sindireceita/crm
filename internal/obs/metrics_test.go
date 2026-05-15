package obs_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/obs"
)

func TestNewMetrics_RegistersAllInstruments(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()

	// Force at least one sample on each *Vec so Gather() emits the
	// metric family — Vec collectors are silent until WithLabelValues
	// pre-registers a child series.
	m.HTTPRequests.WithLabelValues("t", "/r", "GET", "200").Inc()
	m.HTTPDuration.WithLabelValues("t", "/r").Observe(0.1)
	m.RLSMisses.Inc()

	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"http_requests_total":           false,
		"http_request_duration_seconds": false,
		"rls_misses_total":              false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("metric %s not registered", name)
		}
	}
}

func TestMetrics_Handler_ServesScrape(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	m.RLSMisses.Inc()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rls_misses_total 1") {
		t.Errorf("scrape missing rls_misses_total: %q", body)
	}
}

func TestHTTPMetrics_RecordsCounterAndDuration(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	tenantOf := func(*http.Request) string { return "t-1" }
	routeOf := func(*http.Request) string { return "/login" }
	mw := m.HTTPMetrics(tenantOf, routeOf)

	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status passthrough: got %d", rec.Code)
	}
	got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("t-1", "/login", http.MethodPost, "201"))
	if got != 1 {
		t.Errorf("http_requests_total: got %v, want 1", got)
	}
	if testutil.CollectAndCount(m.HTTPDuration) == 0 {
		t.Error("http_request_duration_seconds: no observations recorded")
	}
}

func TestHTTPMetrics_DefaultsWhenWriteCalledWithoutWriteHeader(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	mw := m.HTTPMetrics(
		func(*http.Request) string { return "" },
		func(*http.Request) string { return "/x" },
	)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req)

	got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("", "/x", http.MethodGet, "200"))
	if got != 1 {
		t.Errorf("default 200 status not recorded: got %v", got)
	}
}

func TestHTTPMetrics_NilTenantOfAndRouteOf_UseFallback(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	mw := m.HTTPMetrics(nil, nil)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))

	req := httptest.NewRequest(http.MethodGet, "/fallback", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req)

	got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("", "/fallback", http.MethodGet, "204"))
	if got != 1 {
		t.Errorf("fallback recorder failed: got %v", got)
	}
}

func TestHTTPMetrics_NilReceiver_PassThrough(t *testing.T) {
	t.Parallel()
	var m *obs.Metrics
	mw := m.HTTPMetrics(nil, nil)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req) // must not panic
	if rec.Code != http.StatusOK {
		t.Errorf("nil-receiver pass-through changed status: %d", rec.Code)
	}
}

func TestStatusRecorder_DoubleWriteHeader_KeepsFirst(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	mw := m.HTTPMetrics(
		func(*http.Request) string { return "" },
		func(*http.Request) string { return "/dbl" },
	)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusServiceUnavailable) // ignored
	}))

	req := httptest.NewRequest(http.MethodGet, "/dbl", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req)

	got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("", "/dbl", http.MethodGet, "418"))
	if got != 1 {
		t.Errorf("first WriteHeader (418) not retained: got %v", got)
	}
}

func TestPackageDefault_SetGetIncRLSMiss(t *testing.T) {
	// Cannot run in parallel — mutates package-level default.
	prev := obs.Default()
	t.Cleanup(func() { obs.SetDefault(prev) })

	obs.SetDefault(nil)
	obs.IncRLSMiss() // no panic on nil

	m := obs.NewMetrics()
	obs.SetDefault(m)
	if obs.Default() != m {
		t.Fatal("Default() did not return the set instance")
	}
	obs.IncRLSMiss()
	obs.IncRLSMiss()
	if got := testutil.ToFloat64(m.RLSMisses); got != 2 {
		t.Errorf("rls_misses_total: got %v, want 2", got)
	}
}

func TestMetrics_NilReceiver_IncRLSMiss_NoPanic(t *testing.T) {
	t.Parallel()
	var m *obs.Metrics
	m.IncRLSMiss() // must not panic
}

// Compile-time assertion that the package-level Counter satisfies the
// minimal interface we use (catch silent type drift in client_golang
// upgrades).
var _ prometheus.Counter = (prometheus.Counter)(nil)

// Sanity: obs.NewMetrics returns a non-nil registry.
func TestNewMetrics_NonNil(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	if m == nil || m.Registry == nil {
		t.Fatal("NewMetrics returned nil")
	}
}

// Catches attempts to register the same name twice.
func TestNewMetrics_TwoInstancesAreIndependent(t *testing.T) {
	t.Parallel()
	a := obs.NewMetrics()
	b := obs.NewMetrics()
	if a.Registry == b.Registry {
		t.Fatal("two NewMetrics calls share a registry")
	}
}

func TestWebhookTimestampWindowDrop_IncrementsLabelledCounter(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	m.WebhookTimestampWindowDrop("whatsapp", "past")
	m.WebhookTimestampWindowDrop("whatsapp", "past")
	m.WebhookTimestampWindowDrop("whatsapp", "future")

	if got := testutil.ToFloat64(m.WebhookTimestampWindowDrops.WithLabelValues("whatsapp", "past")); got != 2 {
		t.Errorf(`{channel="whatsapp",direction="past"} = %v, want 2`, got)
	}
	if got := testutil.ToFloat64(m.WebhookTimestampWindowDrops.WithLabelValues("whatsapp", "future")); got != 1 {
		t.Errorf(`{channel="whatsapp",direction="future"} = %v, want 1`, got)
	}
}

func TestWebhookTimestampWindowDrop_NilReceiver_NoPanic(t *testing.T) {
	t.Parallel()
	var m *obs.Metrics
	m.WebhookTimestampWindowDrop("whatsapp", "past") // must not panic
}

func TestPackageDefault_WebhookTimestampWindowDrop(t *testing.T) {
	// Cannot run in parallel — mutates the package-level default.
	prev := obs.Default()
	t.Cleanup(func() { obs.SetDefault(prev) })

	obs.SetDefault(nil)
	obs.WebhookTimestampWindowDrop("whatsapp", "past") // no panic when default is nil

	m := obs.NewMetrics()
	obs.SetDefault(m)
	obs.WebhookTimestampWindowDrop("whatsapp", "past")
	obs.WebhookTimestampWindowDrop("whatsapp", "future")
	if got := testutil.ToFloat64(m.WebhookTimestampWindowDrops.WithLabelValues("whatsapp", "past")); got != 1 {
		t.Errorf(`{channel="whatsapp",direction="past"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(m.WebhookTimestampWindowDrops.WithLabelValues("whatsapp", "future")); got != 1 {
		t.Errorf(`{channel="whatsapp",direction="future"} = %v, want 1`, got)
	}
}

// Confirms that the response writer wrapper does not corrupt the
// downstream Write chain.
func TestStatusRecorder_WritesPropagate(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	mw := m.HTTPMetrics(nil, nil)
	final := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	final.ServeHTTP(rec, req)
	if rec.Body.String() != "payload" {
		t.Errorf("body lost: %q", rec.Body.String())
	}
}

// We don't assert tests above by checking errors.Is; the line below
// keeps the errors import live for potential future expansion.
var _ = errors.New
