package wasession

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
)

// --- fakes -----------------------------------------------------------------

type fakeProvisioner struct {
	mu           sync.Mutex
	snap         SessionSnapshot
	snapErr      error
	connectErr   error
	disconnErr   error
	connectCalls int
	disconnCalls int
}

func (f *fakeProvisioner) Snapshot(_ context.Context, _ uuid.UUID) (SessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.snapErr
}

func (f *fakeProvisioner) Connect(_ context.Context, _ uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCalls++
	return f.connectErr
}

func (f *fakeProvisioner) Disconnect(_ context.Context, _ uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnCalls++
	return f.disconnErr
}

type fakeConsent struct {
	mu        sync.Mutex
	state     ConsentState
	latestErr error
	recordErr error
	recorded  []ConsentInput
}

func (f *fakeConsent) Latest(_ context.Context, _, _ uuid.UUID) (ConsentState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.latestErr
}

func (f *fakeConsent) Record(_ context.Context, in ConsentInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, in)
	// Reflect the grant so a subsequent Latest sees it.
	f.state = ConsentState{Granted: true, Version: in.Version, At: time.Now()}
	return nil
}

// --- harness ---------------------------------------------------------------

func newHandler(t *testing.T, prov Provisioner, gate ConsentGate) *Handler {
	t.Helper()
	h, err := New(Deps{
		Provisioner: prov,
		Consent:     gate,
		UserID:      func(*http.Request) uuid.UUID { return testUser },
		CSRFToken:   func(*http.Request) string { return "csrf-tok" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

var (
	testTenant = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testUser   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func serve(t *testing.T, h *Handler, method, target, body string, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.RemoteAddr = "203.0.113.7:5555"
	if withTenant {
		r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: testTenant, Name: "Acme"}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// --- tests -----------------------------------------------------------------

func TestNew_RequiredDeps(t *testing.T) {
	t.Parallel()
	uid := func(*http.Request) uuid.UUID { return testUser }
	cases := []struct {
		name string
		deps Deps
	}{
		{"no provisioner", Deps{Consent: &fakeConsent{}, UserID: uid}},
		{"no consent", Deps{Provisioner: &fakeProvisioner{}, UserID: uid}},
		{"no userid", Deps{Provisioner: &fakeProvisioner{}, Consent: &fakeConsent{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.deps); err == nil {
				t.Fatal("New err = nil, want error for missing required dep")
			}
		})
	}
}

func TestPage_NotConsented_ShowsConsentForm(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeProvisioner{}, &fakeConsent{})
	rec := serve(t, h, http.MethodGet, BasePath, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="wa-consent-submit"`) {
		t.Error("page missing consent form for non-consented operator")
	}
	if strings.Contains(body, `data-testid="wa-connect"`) {
		t.Error("page must NOT offer connect before consent (deny-by-default)")
	}
	if !strings.Contains(body, "banimento") {
		t.Error("page missing ban-risk notice")
	}
}

func TestPage_Consented_ShowsControls(t *testing.T) {
	t.Parallel()
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: NoticeVersion, At: time.Now()}}
	prov := &fakeProvisioner{snap: SessionSnapshot{Status: "disconnected", Active: true}}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodGet, BasePath, "", true)
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="wa-connect"`) {
		t.Error("consented page missing connect control")
	}
	if !strings.Contains(body, `data-testid="wa-disconnect"`) {
		t.Error("active session missing disconnect control")
	}
}

func TestPage_StaleConsentVersion_TreatedAsNotConsented(t *testing.T) {
	t.Parallel()
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: "old-version", At: time.Now()}}
	h := newHandler(t, &fakeProvisioner{}, gate)
	rec := serve(t, h, http.MethodGet, BasePath, "", true)
	if !strings.Contains(rec.Body.String(), `data-testid="wa-consent-submit"`) {
		t.Error("stale-version grant must require re-consent")
	}
}

func TestPage_MissingTenant_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeProvisioner{}, &fakeConsent{})
	rec := serve(t, h, http.MethodGet, BasePath, "", false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 without tenant", rec.Code)
	}
}

func TestStatusFragment_Pairing_RendersQRAndPolls(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{snap: SessionSnapshot{Status: "pairing", Active: true, QRPayload: "2@abc,def,ghi"}}
	h := newHandler(t, prov, &fakeConsent{})
	rec := serve(t, h, http.MethodGet, BasePath+"/status", "", true)
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Error("pairing status missing inline QR svg")
	}
	if !strings.Contains(body, `hx-trigger="every 3s"`) {
		t.Error("active status fragment must self-poll")
	}
	// The raw pairing payload must never appear as text in the response.
	if strings.Contains(body, "2@abc,def,ghi") {
		t.Error("QR payload leaked verbatim into the response body")
	}
}

func TestStatusFragment_Banned_NoPoll(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{snap: SessionSnapshot{Status: "banned", Active: true}}
	h := newHandler(t, prov, &fakeConsent{})
	rec := serve(t, h, http.MethodGet, BasePath+"/status", "", true)
	if strings.Contains(rec.Body.String(), "hx-trigger") {
		t.Error("banned (terminal) status must not poll")
	}
}

func TestStatusFragment_SnapshotError_Degrades(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{snapErr: errors.New("boom")}
	h := newHandler(t, prov, &fakeConsent{})
	rec := serve(t, h, http.MethodGet, BasePath+"/status", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 degraded", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hx-trigger") {
		t.Error("error status must not poll")
	}
}

func TestRecordConsent_Success(t *testing.T) {
	t.Parallel()
	gate := &fakeConsent{}
	h := newHandler(t, &fakeProvisioner{}, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/consent", "accept_risk=on", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(gate.recorded) != 1 {
		t.Fatalf("recorded %d consents, want 1", len(gate.recorded))
	}
	got := gate.recorded[0]
	if got.Version != NoticeVersion || got.TenantID != testTenant || got.UserID != testUser {
		t.Errorf("recorded = %+v, want tenant/user/version populated", got)
	}
	if got.IP.String() != "203.0.113.7" {
		t.Errorf("recorded IP = %s, want 203.0.113.7", got.IP)
	}
	if !strings.Contains(rec.Body.String(), `data-testid="wa-connect"`) {
		t.Error("after consent the panel should show controls")
	}
}

func TestRecordConsent_MissingCheckbox_Rejected(t *testing.T) {
	t.Parallel()
	gate := &fakeConsent{}
	h := newHandler(t, &fakeProvisioner{}, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/consent", "", true)
	if len(gate.recorded) != 0 {
		t.Fatal("consent recorded without the checkbox checked")
	}
	if !strings.Contains(rec.Body.String(), "necessário marcar") {
		t.Error("missing validation message for unchecked consent")
	}
}

func TestRecordConsent_StoreError(t *testing.T) {
	t.Parallel()
	gate := &fakeConsent{recordErr: errors.New("db down")}
	h := newHandler(t, &fakeProvisioner{}, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/consent", "accept_risk=on", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 degraded", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Falha ao registrar") {
		t.Error("missing record-failure message")
	}
}

func TestConnect_WithoutConsent_Denied(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{}
	gate := &fakeConsent{} // no grant
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/connect", "", true)
	if prov.connectCalls != 0 {
		t.Fatal("Connect called without recorded consent (deny-by-default breach)")
	}
	if !strings.Contains(rec.Body.String(), "Registre o consentimento") {
		t.Error("missing consent-required message on connect")
	}
}

func TestConnect_WithConsent_Activates(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{snap: SessionSnapshot{Status: "pairing", Active: true, QRPayload: "2@x"}}
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: NoticeVersion, At: time.Now()}}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/connect", "", true)
	if prov.connectCalls != 1 {
		t.Fatalf("Connect calls = %d, want 1", prov.connectCalls)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Error("after connect the pairing QR should render")
	}
}

func TestConnect_ProvisionerError(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{connectErr: errors.New("nope")}
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: NoticeVersion, At: time.Now()}}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/connect", "", true)
	if !strings.Contains(rec.Body.String(), "Falha ao ativar") {
		t.Error("missing connect-failure message")
	}
}

func TestConnect_ConsentLookupError(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{}
	gate := &fakeConsent{latestErr: errors.New("down")}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/connect", "", true)
	if prov.connectCalls != 0 {
		t.Fatal("Connect attempted despite consent lookup failure")
	}
	if !strings.Contains(rec.Body.String(), "verificar o consentimento") {
		t.Error("missing consent-verification-failure message")
	}
}

func TestDisconnect_Success(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{snap: SessionSnapshot{Status: "", Active: false}}
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: NoticeVersion, At: time.Now()}}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/disconnect", "", true)
	if prov.disconnCalls != 1 {
		t.Fatalf("Disconnect calls = %d, want 1", prov.disconnCalls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestDisconnect_Error(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{disconnErr: errors.New("nope")}
	gate := &fakeConsent{state: ConsentState{Granted: true, Version: NoticeVersion, At: time.Now()}}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodPost, BasePath+"/disconnect", "", true)
	if !strings.Contains(rec.Body.String(), "Falha ao desconectar") {
		t.Error("missing disconnect-failure message")
	}
}

func TestBuildPanel_ConsentLookupError_FailsClosed(t *testing.T) {
	t.Parallel()
	prov := &fakeProvisioner{}
	gate := &fakeConsent{latestErr: errors.New("down")}
	h := newHandler(t, prov, gate)
	rec := serve(t, h, http.MethodGet, BasePath, "", true)
	body := rec.Body.String()
	if strings.Contains(body, `data-testid="wa-connect"`) {
		t.Error("consent lookup failure must fail closed (no controls)")
	}
	if !strings.Contains(body, "verificar o consentimento") {
		t.Error("missing consent-error notice on fail-closed panel")
	}
}
