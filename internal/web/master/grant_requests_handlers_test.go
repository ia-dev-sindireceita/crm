package master_test

// SIN-63605 — tests for the master 4-eyes grant-request HTMX surface.
// Unit tests cover the five handlers (create / list / show / approve /
// reject), the 422 path on actor==requester, the 409 path on
// already-decided, the 503 path when the GrantRequests port is nil,
// and the HTMX partial vs full-page rendering.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- Fake GrantRequestPort -------------------------------------------

type fakeGrantRequests struct {
	mu sync.Mutex

	rows map[uuid.UUID]master.GrantRequest

	// knobs the tests flip per scenario.
	createErr  error
	listErr    error
	getErr     error
	approveErr error
	rejectErr  error

	// capture
	lastCreate  master.CreateGrantRequestInput
	lastApprove master.DecideGrantRequestInput
	lastReject  master.DecideGrantRequestInput

	createdGrants []master.GrantRow
	calls         atomic.Int64
}

func newFakeGrantRequests() *fakeGrantRequests {
	return &fakeGrantRequests{rows: map[uuid.UUID]master.GrantRequest{}}
}

func (f *fakeGrantRequests) CreateGrantRequest(_ context.Context, in master.CreateGrantRequestInput) (master.GrantRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.Add(1)
	f.lastCreate = in
	if f.createErr != nil {
		return master.GrantRequest{}, f.createErr
	}
	req := master.GrantRequest{
		ID:          uuid.New(),
		ExternalID:  fmt.Sprintf("01ULIDREQ%d", f.calls.Load()),
		TenantID:    in.TenantID,
		Kind:        in.Kind,
		PeriodDays:  in.PeriodDays,
		Amount:      in.Amount,
		Reason:      in.Reason,
		CreatedByID: in.ActorUserID,
		State:       master.GrantRequestStateAwaiting,
		CreatedAt:   time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
	}
	f.rows[req.ID] = req
	return req, nil
}

func (f *fakeGrantRequests) ListAwaitingRequests(_ context.Context) ([]master.GrantRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]master.GrantRequest, 0, len(f.rows))
	for _, r := range f.rows {
		if r.State == master.GrantRequestStateAwaiting {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeGrantRequests) GetGrantRequest(_ context.Context, id uuid.UUID) (master.GrantRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return master.GrantRequest{}, f.getErr
	}
	if r, ok := f.rows[id]; ok {
		return r, nil
	}
	return master.GrantRequest{}, master.ErrGrantRequestNotFound
}

func (f *fakeGrantRequests) ApproveGrantRequest(_ context.Context, in master.DecideGrantRequestInput) (master.GrantRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastApprove = in
	if f.approveErr != nil {
		return master.GrantRow{}, f.approveErr
	}
	req, ok := f.rows[in.RequestID]
	if !ok {
		return master.GrantRow{}, master.ErrGrantRequestNotFound
	}
	if req.State != master.GrantRequestStateAwaiting {
		return master.GrantRow{}, master.ErrGrantRequestAlreadyDecided
	}
	if req.CreatedByID == in.ActorUserID {
		return master.GrantRow{}, master.ErrGrantRequestApproverIsCreator
	}
	now := time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC)
	req.State = master.GrantRequestStateApproved
	req.SecondApproverID = in.ActorUserID
	req.DecidedAt = now
	f.rows[in.RequestID] = req
	row := master.GrantRow{
		ID:          uuid.New(),
		ExternalID:  fmt.Sprintf("01ULIDGRANT%d", f.calls.Add(1)),
		TenantID:    req.TenantID,
		Kind:        req.Kind,
		PeriodDays:  req.PeriodDays,
		Amount:      req.Amount,
		Reason:      req.Reason,
		CreatedByID: req.CreatedByID,
		CreatedAt:   now,
	}
	f.createdGrants = append(f.createdGrants, row)
	return row, nil
}

func (f *fakeGrantRequests) RejectGrantRequest(_ context.Context, in master.DecideGrantRequestInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReject = in
	if f.rejectErr != nil {
		return f.rejectErr
	}
	req, ok := f.rows[in.RequestID]
	if !ok {
		return master.ErrGrantRequestNotFound
	}
	if req.State != master.GrantRequestStateAwaiting {
		return master.ErrGrantRequestAlreadyDecided
	}
	if req.CreatedByID == in.ActorUserID {
		return master.ErrGrantRequestApproverIsCreator
	}
	req.State = master.GrantRequestStateRejected
	req.SecondApproverID = in.ActorUserID
	req.DecidedAt = time.Date(2026, 5, 27, 13, 30, 0, 0, time.UTC)
	f.rows[in.RequestID] = req
	return nil
}

// ----- helpers ----------------------------------------------------------

func newGrantRequestsHandler(t *testing.T, port master.GrantRequestPort) (*master.Handler, *fakeGrantRequests) {
	t.Helper()
	var fp *fakeGrantRequests
	if port == nil {
		fp = newFakeGrantRequests()
		port = fp
	}
	deps := master.Deps{
		Tenants:       &stubLister{},
		Creator:       &stubCreator{},
		Plans:         &stubPlans{},
		Assigner:      &stubAssigner{},
		CSRFToken:     func(*http.Request) string { return "csrf-test-token" },
		Logger:        discardLogger(),
		GrantRequests: port,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	if fp == nil {
		return h, nil
	}
	return h, fp
}

func grantRequestsMux(t *testing.T, h *master.Handler) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// ----- 503 fail-fast ----------------------------------------------------

func TestGrantRequests_NilPort_503OnEveryRoute(t *testing.T) {
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "x" },
		Logger:    discardLogger(),
		// GrantRequests intentionally nil.
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := grantRequestsMux(t, h)

	reqID := uuid.New().String()
	routes := []struct {
		name, method, target, body string
	}{
		{"create", http.MethodPost, "/master/tenants/" + fakeTenantID + "/grants/requests", issueFormBody("extra_tokens", "", "5000000", "reason text for cap exceed")},
		{"list", http.MethodGet, "/master/grants/requests", ""},
		{"show", http.MethodGet, "/master/grants/requests/" + reqID, ""},
		{"approve", http.MethodPost, "/master/grants/requests/" + reqID + "/approve", ""},
		{"reject", http.MethodPost, "/master/grants/requests/" + reqID + "/reject", ""},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMaster(r.method, r.target, r.body))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s status = %d, want 503 (body=%s)", r.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// ----- CreateGrantRequest -----------------------------------------------

func TestCreateGrantRequest_HappyPath_RedirectsToDetail(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants/requests",
		issueFormBody("extra_tokens", "", "20000000", "cap-exceeded grant explanation"),
	))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/master/grants/requests/") {
		t.Errorf("Location = %q, want /master/grants/requests/{id}", loc)
	}
	if got, want := fp.lastCreate.Amount, int64(20_000_000); got != want {
		t.Errorf("captured Amount = %d, want %d", got, want)
	}
	if got, want := fp.lastCreate.Kind, master.GrantKindExtraTokens; got != want {
		t.Errorf("captured Kind = %s, want %s", got, want)
	}
}

func TestCreateGrantRequest_HXRequest_EmitsHXRedirect(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	req := reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants/requests",
		issueFormBody("free_subscription_period", "365", "", "long incident triage credit"),
	)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); !strings.HasPrefix(got, "/master/grants/requests/") {
		t.Errorf("HX-Redirect = %q, want /master/grants/requests/{id}", got)
	}
}

func TestCreateGrantRequest_InvalidForm_422(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants/requests",
		issueFormBody("extra_tokens", "", "0", "valid reason text"),
	))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestCreateGrantRequest_InvalidTenantID_400(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/not-a-uuid/grants/requests", "",
	))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ----- ListGrantRequests ------------------------------------------------

func TestListGrantRequests_RendersAwaitingRows(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	// Seed via the port.
	seedReq, err := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      15_000_000,
		Reason:      "seed reason for list",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/grants/requests", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		"grant-requests-title",
		"id=\"grant-requests-panel\"",
		seedReq.ID.String(),
		"Revisar",
		"Tokens extras",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestListGrantRequests_HXRequest_RendersPartialOnly(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	req := reqWithMaster(http.MethodGet, "/master/grants/requests", "")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX-Request should return partial, got full layout")
	}
	if !strings.Contains(body, "id=\"grant-requests-panel\"") {
		t.Errorf("partial missing #grant-requests-panel")
	}
	if !strings.Contains(body, "Nenhuma solicitação aguardando aprovação") {
		t.Errorf("empty list should render placeholder; body=%s", body)
	}
}

// ----- ShowGrantRequest -------------------------------------------------

func TestShowGrantRequest_RendersDetailWithForms(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	req, _ := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindFreeSubscriptionPeriod,
		PeriodDays:  365,
		Reason:      "long-period courtesy reason",
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/grants/requests/"+req.ID.String(), ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		req.ID.String(),
		"action=\"/master/grants/requests/" + req.ID.String() + "/approve\"",
		"action=\"/master/grants/requests/" + req.ID.String() + "/reject\"",
		"csrf-test-token",
		"long-period courtesy reason",
		"Aprovar",
		"Rejeitar",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestShowGrantRequest_NotFound_404(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/grants/requests/"+uuid.NewString(), ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestShowGrantRequest_InvalidID_400(t *testing.T) {
	h, _ := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/grants/requests/not-a-uuid", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ----- Approve happy + 422 + 409 ----------------------------------------

func TestApproveGrantRequest_HappyPath(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	requesterID := uuid.New()
	req, _ := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      50_000_000,
		Reason:      "happy path approve test",
	})

	rec := httptest.NewRecorder()
	// masterPrincipal() returns UserID 11111111-..., different from requesterID.
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+req.ID.String()+"/approve", "",
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(fp.createdGrants); got != 1 {
		t.Fatalf("createdGrants len = %d, want 1", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Solicitação aprovada e grant emitida.") {
		t.Errorf("body missing flash; body=%s", body)
	}
	if !strings.Contains(body, "Aprovada") {
		t.Errorf("body missing approved-state label")
	}
}

func TestApproveGrantRequest_ActorIsRequester_422(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	// Use the master principal UUID so actor==requester.
	requesterID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req, _ := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      50_000_000,
		Reason:      "self approval attempt",
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+req.ID.String()+"/approve", "",
	))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "aprovador deve ser um usuário diferente do solicitante") {
		t.Errorf("body missing 4-eyes error message; body=%s", body)
	}
	if len(fp.createdGrants) != 0 {
		t.Errorf("createdGrants should be empty on 422; got %d", len(fp.createdGrants))
	}
}

func TestApproveGrantRequest_AlreadyDecided_409(t *testing.T) {
	fp := newFakeGrantRequests()
	fp.approveErr = master.ErrGrantRequestAlreadyDecided
	requesterID := uuid.New()
	req, _ := fp.CreateGrantRequest(context.Background(), master.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      50_000_000,
		Reason:      "already decided race",
	})

	h, _ := newGrantRequestsHandler(t, fp)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+req.ID.String()+"/approve", "",
	))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "já foi decidida") {
		t.Errorf("body missing already-decided message; body=%s", body)
	}
}

func TestApproveGrantRequest_PortReturnsNotFound_404(t *testing.T) {
	fp := newFakeGrantRequests()
	fp.approveErr = master.ErrGrantRequestNotFound

	h, _ := newGrantRequestsHandler(t, fp)
	mux := grantRequestsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+uuid.NewString()+"/approve", "",
	))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ----- Reject -----------------------------------------------------------

func TestRejectGrantRequest_HappyPath(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	requesterID := uuid.New()
	req, _ := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      30_000_000,
		Reason:      "happy reject path test",
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+req.ID.String()+"/reject", "",
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Solicitação rejeitada.") {
		t.Errorf("body missing reject flash; body=%s", rec.Body.String())
	}
	if len(fp.createdGrants) != 0 {
		t.Errorf("reject must not emit a grant; got %d", len(fp.createdGrants))
	}
}

func TestRejectGrantRequest_ActorIsRequester_422(t *testing.T) {
	h, fp := newGrantRequestsHandler(t, nil)
	mux := grantRequestsMux(t, h)

	requesterID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req, _ := fp.CreateGrantRequest(t.Context(), master.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    uuid.MustParse(fakeTenantID),
		Kind:        master.GrantKindExtraTokens,
		Amount:      30_000_000,
		Reason:      "self reject attempt",
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/requests/"+req.ID.String()+"/reject", "",
	))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// ----- 4-eyes button surfaces from cap-exceeded grant POST --------------

func TestIssueGrant_PerGrantCapExceeded_RendersFourEyesButton(t *testing.T) {
	fg := newFakeGrants()
	fg.issueErr = master.ErrPerGrantCapExceeded

	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "20000000", "above per-grant cap reason"),
	))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Solicitar aprovação 4-eyes",
		"formaction=\"/master/tenants/" + fakeTenantID + "/grants/requests\"",
		"hx-post=\"/master/tenants/" + fakeTenantID + "/grants/requests\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
}

func TestIssueGrant_PerTenantWindowCapExceeded_RendersFourEyesButton(t *testing.T) {
	fg := newFakeGrants()
	fg.issueErr = master.ErrPerTenantWindowCapExceeded

	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "9000000", "365d window exceeded reason"),
	))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Solicitar aprovação 4-eyes") {
		t.Errorf("body missing 4-eyes button on window cap; body=%s", body)
	}
}

// ----- HTMX form-body helper compatibility check ------------------------

// guarantee that the helper we share with grants_handlers_test.go
// builds the urlencoded form we expect.
func TestIssueFormBody_Shape(t *testing.T) {
	got := issueFormBody("extra_tokens", "", "100", "expand the unit test fixture body")
	if got == "" {
		t.Fatal("issueFormBody returned empty string")
	}
	if want := "kind=extra_tokens"; !strings.Contains(got, want) {
		t.Errorf("body missing %q", want)
	}
	if want := "amount=100"; !strings.Contains(got, want) {
		t.Errorf("body missing %q", want)
	}
}

// guarantee that url.Values encoding does what we think (regression
// canary so the assertions above stay stable).
func TestURLValuesPreservesOrderIndependent(t *testing.T) {
	v := url.Values{"k": []string{"v"}}
	if got := v.Encode(); got != "k=v" {
		t.Fatalf("encode = %q, want %q", got, "k=v")
	}
}

// guarantee fakeGrantRequests reports an error sentinel when wired
// for it (defends against a regression where a future refactor turns
// errors into nil).
func TestFakeGrantRequests_PassesThroughCreateErr(t *testing.T) {
	fp := newFakeGrantRequests()
	fp.createErr = errors.New("synthetic db down")
	_, err := fp.CreateGrantRequest(context.Background(), master.CreateGrantRequestInput{})
	if err == nil || !strings.Contains(err.Error(), "synthetic db down") {
		t.Fatalf("expected synthetic db down err, got %v", err)
	}
}
