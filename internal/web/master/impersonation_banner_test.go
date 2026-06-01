package master_test

// SIN-63956 / master-impersonation-spec rev 2 — UI tests for the
// impersonation banner, audit-feed chip, tenant detail page with the
// impersonate trigger, and the 4-eyes confirm-twice + self-approve UI
// guard. Mirrors §5.5 (security) + §10.8 (UX) test checklists.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/master"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// ----- helpers ----------------------------------------------------------

type stubTenantsByID struct {
	tenant *tenancy.Tenant
	err    error
}

func (s *stubTenantsByID) ResolveByID(_ context.Context, id uuid.UUID) (*tenancy.Tenant, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.tenant != nil {
		out := *s.tenant
		out.ID = id
		return &out, nil
	}
	return nil, tenancy.ErrTenantNotFound
}

func bannerCtx(t *testing.T, name, slug string) (context.Context, uuid.UUID) {
	t.Helper()
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	sess := &impersonation.Session{
		ID:              uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		MasterUserID:    uuid.MustParse("55555555-5555-5555-5555-555555555555"),
		MasterSessionID: uuid.MustParse("66666666-6666-6666-6666-666666666666"),
		TargetTenantID:  id,
		Reason:          "ticket #11324",
		StartedAt:       time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2026, 5, 31, 13, 15, 0, 0, time.UTC),
	}
	ctx := middleware.WithActiveImpersonationForTest(context.Background(), sess)
	return ctx, sess.TargetTenantID
}

func discardSlog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ----- §5.5 #6 + §10.8 #34 — banner_xss_test ----------------------------

func TestImpersonationBanner_EscapesTenantDisplayName(t *testing.T) {
	t.Parallel()
	ctx, _ := bannerCtx(t, "<script>alert(1)</script>", "evil-slug")
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil).WithContext(ctx)
	resolver := &stubTenantsByID{tenant: &tenancy.Tenant{Name: `<script>alert("xss")</script>`, Host: "evil"}}
	bctx := master.BuildImpersonationContext(r, resolver, "csrf-token", func() time.Time { return time.Now().UTC() })
	if bctx == nil {
		t.Fatal("expected context, got nil")
	}
	// Render via the master tenants page (full layout): the banner
	// must HTML-escape the display name.
	rendered := renderTenantsListWithBanner(t, bctx)
	if !strings.Contains(rendered, `Tenant: <strong>&lt;script&gt;alert(&#34;xss&#34;)&lt;/script&gt;</strong>`) {
		t.Fatalf("banner did not escape tenant name; got: %q", rendered)
	}
	if strings.Contains(rendered, `<script>alert("xss")</script>`) {
		t.Fatal("banner emitted unescaped <script> tag")
	}
}

// ----- §10.8 #36 — banner_aaa_contrast_test -----------------------------

func TestImpersonationBanner_PinnedAAATokens(t *testing.T) {
	t.Parallel()
	// Smoke-test the master.css file directly: the banner-scoped
	// tokens are the AAA-pinned hexes resolved in spec §6.2.
	css := readMasterCSS(t)
	for _, want := range []string{
		"--color-impersonation-bg: #991b1b",
		"--color-impersonation-fg: #ffffff",
		"--color-impersonation-rim: #7f1d1d",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("master.css missing token %q", want)
		}
	}
	// Global --color-danger must NOT be mutated by the banner stylesheet.
	if strings.Contains(css, "--color-danger:") && !strings.Contains(css, "/* keep upstream --color-danger") {
		// Allow comments referencing the token; reject any rule that
		// redeclares it at the global :root selector.
		idx := strings.Index(css, "--color-danger:")
		// Look backward for a :root selector — if found, fail.
		head := css[:idx]
		if strings.LastIndex(head, ":root") > strings.LastIndex(head, ".shell__impersonation-banner") {
			t.Fatal("master.css must not mutate global --color-danger at :root")
		}
	}
}

// ----- §10.8 #35 — banner_present_on_all_master_routes_test -------------

func TestImpersonationBanner_PresentOnAllMasterPages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		view interface{}
	}{
		{"tenants_list", makeTenantsPageData()},
		{"tenant_detail", makeTenantDetailData()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := renderPage(t, tc.name, tc.view)
			if !strings.Contains(rendered, `class="shell__impersonation-banner"`) {
				t.Fatalf("%s: banner partial missing from rendered output", tc.name)
			}
			if !strings.Contains(rendered, `data-impersonation-banner="true"`) {
				t.Fatalf("%s: banner data attribute missing", tc.name)
			}
			if !strings.Contains(rendered, `SAIR DA IMPERSONAÇÃO`) {
				t.Fatalf("%s: SAIR button missing", tc.name)
			}
		})
	}
}

func TestImpersonationBanner_HiddenWhenNoEnvelope(t *testing.T) {
	t.Parallel()
	view := makeTenantsPageDataNoBanner()
	rendered := renderPage(t, "tenants_list", view)
	if strings.Contains(rendered, "shell__impersonation-banner") {
		t.Fatal("banner rendered when ActiveImpersonation is nil")
	}
	if !strings.Contains(rendered, "<body ") {
		t.Fatal("rendered page does not look like a full layout")
	}
	if strings.Contains(rendered, `data-impersonating="true"`) {
		t.Fatal("body must not carry data-impersonating when no envelope")
	}
}

// ----- §10.8 #37 — countdown_attrs_test ---------------------------------

func TestImpersonationBanner_CountdownDataAttributes(t *testing.T) {
	t.Parallel()
	ctx, _ := bannerCtx(t, "Acme", "acme")
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil).WithContext(ctx)
	now := time.Date(2026, 5, 31, 13, 5, 0, 0, time.UTC)
	resolver := &stubTenantsByID{tenant: &tenancy.Tenant{Name: "Acme Saúde Ltda", Host: "acme.crm.local"}}
	bctx := master.BuildImpersonationContext(r, resolver, "tk", func() time.Time { return now })
	rendered := renderTenantsListWithBanner(t, bctx)
	// expires_at = 13:15:00Z (StartedAt + 15min); server_now = 13:05Z.
	if !strings.Contains(rendered, `data-expires-at="2026-05-31T13:15:00Z"`) {
		t.Fatalf("data-expires-at missing or wrong: %q", excerpt(rendered, "data-expires-at"))
	}
	if !strings.Contains(rendered, `data-server-now="2026-05-31T13:05:00Z"`) {
		t.Fatalf("data-server-now missing or wrong: %q", excerpt(rendered, "data-server-now"))
	}
	// Reason rendered + truncated when long (test the helper directly).
	if !strings.Contains(rendered, "Motivo: ticket #11324") {
		t.Fatalf("reason missing in banner: %q", rendered)
	}
}

// ----- §10.8 #38 — audit_feed_collapsed_default_test --------------------

func TestImpersonationBanner_AuditFeedChipCollapsedByDefault(t *testing.T) {
	t.Parallel()
	view := makeTenantsPageData()
	rendered := renderPage(t, "tenants_list", view)
	if !strings.Contains(rendered, `class="master-audit-feed__chip"`) {
		t.Fatal("audit feed chip not rendered")
	}
	if !strings.Contains(rendered, `aria-expanded="false"`) {
		t.Fatal("audit feed chip not collapsed by default (aria-expanded != false)")
	}
}

// ----- §10.8 #39 — four_eyes_self_approve_ui_guard_test -----------------

func TestGrantRequestDetail_RendersSelfApproveGuard(t *testing.T) {
	t.Parallel()
	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	req := master.GrantRequest{
		ID:          uuid.New(),
		CreatedByID: requester,
		State:       master.GrantRequestStateAwaiting,
		Kind:        master.GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "diff vs cap test",
		CreatedAt:   time.Now().UTC(),
	}
	// Same user reviewing their own request — guard must render and
	// approve/reject forms MUST NOT.
	rendered := renderGrantRequestDetailPanel(t, req, requester, "")
	if !strings.Contains(rendered, `data-self-approve-guard="true"`) {
		t.Fatal("self-approve guard banner missing for own request")
	}
	if strings.Contains(rendered, `data-approve-trigger="true"`) {
		t.Fatal("approve button must not render for own request (defense in depth on top of 422)")
	}
	// SIN-63980 — spec §4.3 requires hiding both forms entirely; no
	// approve/reject <form action="…"> markup may leak into the DOM
	// for the self-creator branch.
	if regexp.MustCompile(`action="/master/grants/requests/[^"]+/approve"`).MatchString(rendered) {
		t.Fatal("approve form markup must not render for self-creator (spec §4.3 — hide both forms entirely)")
	}
	if regexp.MustCompile(`action="/master/grants/requests/[^"]+/reject"`).MatchString(rendered) {
		t.Fatal("reject form markup must not render for self-creator (spec §4.3 — hide both forms entirely)")
	}
}

func TestGrantRequestDetail_ConfirmTwiceModalOnApproveStage(t *testing.T) {
	t.Parallel()
	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reviewer := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	req := master.GrantRequest{
		ID:          uuid.New(),
		CreatedByID: requester,
		State:       master.GrantRequestStateAwaiting,
		Kind:        master.GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "needs confirm",
		CreatedAt:   time.Now().UTC(),
	}
	// stage=confirm switches the panel into the modal.
	rendered := renderGrantRequestDetailPanel(t, req, reviewer, "confirm")
	if !strings.Contains(rendered, `data-confirm-modal="true"`) {
		t.Fatal("confirm modal missing in stage=confirm render")
	}
	if !strings.Contains(rendered, `data-confirm-final="true"`) {
		t.Fatal("confirm-final button missing")
	}
	// Cancel must be a no-op back to the detail page (default focus).
	if !strings.Contains(rendered, `autofocus`) {
		t.Fatal("cancel button should default-focus on Cancel (spec §10.4 #19)")
	}
	// Final submit must carry the hidden confirm=yes field.
	if !strings.Contains(rendered, `name="confirm" value="yes"`) {
		t.Fatal("confirm-final form missing confirm=yes payload")
	}
}

// ----- §9.5 + tenant_create_wizard_step_back proxy (single-step today)  -

func TestTenantDetail_ImpersonateTriggerRendersAndOpensModal(t *testing.T) {
	t.Parallel()
	// Without ?impersonate=ask, render the CTA.
	view := master.ExportNewTenantDetailData(master.TenantRow{
		ID:   uuid.New(),
		Name: "Acme Saúde Ltda",
		Host: "acme.crm.local",
	}, false, nil)
	rendered := renderPage(t, "tenant_detail", view)
	if !strings.Contains(rendered, `data-impersonate-trigger="true"`) {
		t.Fatal("impersonate trigger missing on tenant detail page")
	}
	if strings.Contains(rendered, `data-impersonate-form="true"`) {
		t.Fatal("modal must NOT render before ?impersonate=ask query")
	}

	// With ShowReasonModal=true, the inline reason form must render.
	view2 := master.ExportNewTenantDetailData(master.TenantRow{
		ID:   uuid.New(),
		Name: "Acme",
		Host: "acme.crm.local",
	}, true, nil)
	rendered2 := renderPage(t, "tenant_detail", view2)
	if !strings.Contains(rendered2, `data-impersonate-form="true"`) {
		t.Fatal("impersonation reason form missing when ShowReasonModal=true")
	}
	if !strings.Contains(rendered2, `<textarea id="impersonate-reason"`) {
		t.Fatal("reason textarea missing")
	}
	if !strings.Contains(rendered2, `minlength="8"`) {
		t.Fatal("reason minlength=8 enforcement missing")
	}
	if !strings.Contains(rendered2, `data-impersonate-cancel="true"`) {
		t.Fatal("cancel link missing")
	}
}

// ----- spec §5.5 #7 — banner present on tenant detail when active ------

func TestTenantDetail_RendersBannerWhenActive(t *testing.T) {
	t.Parallel()
	bctx := &shell.ImpersonationContext{
		TenantName: "Acme Saúde Ltda",
		TenantSlug: "acme.crm.local",
		Reason:     "ticket #11324",
		ExpiresAt:  time.Date(2026, 5, 31, 13, 15, 0, 0, time.UTC),
		ServerNow:  time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
	}
	view := master.ExportNewTenantDetailData(master.TenantRow{
		ID:   uuid.New(),
		Name: "Acme Saúde Ltda",
		Host: "acme.crm.local",
	}, false, bctx)
	rendered := renderPage(t, "tenant_detail", view)
	if !strings.Contains(rendered, "shell__impersonation-banner") {
		t.Fatal("banner missing on tenant detail when ActiveImpersonation set")
	}
	if !strings.Contains(rendered, `data-impersonating="true"`) {
		t.Fatal("body marker missing on tenant detail")
	}
}

// ----- §4.4 + §10.4 #19 confirm-twice — modal renders on GET ?stage=confirm

// The confirm-twice UX is implemented via the GET detail-page render
// path: the "Aprovar…" trigger is a GET link to
// /master/grants/requests/{id}?stage=confirm; that GET returns the
// detail page with the confirm modal in the right-hand REVISOR column.
// The POST /approve only mutates after the operator clicks the modal's
// CONFIRMAR APROVAÇÃO button — i.e. only the SECOND submit lands on
// the mutating endpoint. Spec §5.2 ("Modal open/close on Approve
// button — Server still requires a fresh POST; closing the modal does
// nothing") clarifies the modal is a UX layer; the POST is the
// security boundary, and 4-eyes is the actual control.
func TestApprove_ConfirmStageRendersModal(t *testing.T) {
	t.Parallel()
	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reviewer := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	req := master.GrantRequest{
		ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		ExternalID:  "req_test",
		CreatedByID: requester,
		State:       master.GrantRequestStateAwaiting,
		Kind:        master.GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "diff vs cap test",
		CreatedAt:   time.Now().UTC(),
	}
	store := &stubGrantRequests{get: req}
	h := newHandlerWithGrantRequests(t, store)
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/master/grants/requests/"+req.ID.String()+"?stage=confirm", nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   reviewer,
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?stage=confirm status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `data-confirm-modal="true"`) {
		t.Fatalf("GET ?stage=confirm must render confirm modal, got: %s", out)
	}
	if !strings.Contains(out, `data-confirm-final="true"`) {
		t.Fatal("confirm modal missing CONFIRMAR APROVAÇÃO button")
	}
	if store.approvedCount != 0 {
		t.Fatalf("ApproveGrantRequest must NOT run on GET ?stage=confirm; got %d", store.approvedCount)
	}
}

// ----- ShowTenantDetail + loadTenantDetail ------------------------------

func TestShowTenantDetail_RendersImpersonateTrigger(t *testing.T) {
	t.Parallel()
	row := master.TenantRow{
		ID:   uuid.MustParse("12345678-1234-1234-1234-123456789abc"),
		Name: "Acme Saúde",
		Host: "acme.crm.local",
	}
	lister := &stubLister{res: master.ListResult{Tenants: []master.TenantRow{row}, Page: 1, PageSize: 25, TotalCount: 1}}
	deps := master.Deps{
		Tenants:   lister,
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardSlog(),
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/master/tenants/"+row.ID.String(), nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Impersonar tenant") {
		t.Fatal("detail page missing Impersonar tenant CTA")
	}
	if !strings.Contains(body, "Acme Saúde") {
		t.Fatal("detail page missing tenant display name")
	}
	if !strings.Contains(body, `data-impersonate-trigger="true"`) {
		t.Fatal("detail page missing impersonate trigger data attribute")
	}
}

func TestShowTenantDetail_ReasonModalOnQuery(t *testing.T) {
	t.Parallel()
	row := master.TenantRow{
		ID:   uuid.MustParse("12345678-1234-1234-1234-123456789abc"),
		Name: "Acme Saúde",
		Host: "acme.crm.local",
	}
	lister := &stubLister{res: master.ListResult{Tenants: []master.TenantRow{row}, Page: 1, PageSize: 25, TotalCount: 1}}
	deps := master.Deps{
		Tenants:   lister,
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardSlog(),
	}
	h, _ := master.New(deps)
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/master/tenants/"+row.ID.String()+"?impersonate=ask", nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-impersonate-form="true"`) {
		t.Fatal("reason modal not rendered when impersonate=ask")
	}
	if !strings.Contains(body, `name="reason"`) {
		t.Fatal("reason field missing")
	}
	if !strings.Contains(body, `data-impersonate-cancel="true"`) {
		t.Fatal("cancel link missing")
	}
}

func TestShowTenantDetail_NotFound(t *testing.T) {
	t.Parallel()
	lister := &stubLister{res: master.ListResult{Tenants: []master.TenantRow{}, Page: 1, PageSize: 25, TotalCount: 0}}
	deps := master.Deps{
		Tenants:   lister,
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardSlog(),
	}
	h, _ := master.New(deps)
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/master/tenants/"+uuid.NewString(), nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestShowTenantDetail_DedicatedReaderWins(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	row := master.TenantRow{ID: id, Name: "Detail Tenant", Host: "detail.crm.local"}
	deps := master.Deps{
		Tenants:      &stubLister{res: master.ListResult{}},
		Creator:      &stubCreator{},
		Plans:        &stubPlans{},
		Assigner:     &stubAssigner{},
		CSRFToken:    func(*http.Request) string { return "csrf-test-token" },
		Logger:       discardSlog(),
		TenantDetail: stubTenantDetail{detail: master.TenantDetail{Tenant: row}},
	}
	h, _ := master.New(deps)
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/master/tenants/"+id.String(), nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Detail Tenant") {
		t.Fatal("TenantDetail reader was not used as primary source")
	}
}

type stubTenantDetail struct {
	detail master.TenantDetail
	err    error
}

func (s stubTenantDetail) GetDetail(_ context.Context, _ uuid.UUID) (master.TenantDetail, error) {
	if s.err != nil {
		return master.TenantDetail{}, s.err
	}
	return s.detail, nil
}

// ----- Small banner helpers (formatImpersonationISO, truncate, etc.) ---

func TestFormatImpersonationISO_ZeroTimeYieldsEmpty(t *testing.T) {
	t.Parallel()
	if got := master.ExportFormatImpersonationISO(time.Time{}); got != "" {
		t.Fatalf("expected empty string for zero time, got %q", got)
	}
}

func TestTruncateImpersonationReason_BothBranches(t *testing.T) {
	t.Parallel()
	short := "short"
	if got := master.ExportTruncateImpersonationReason(short); got != short {
		t.Fatalf("short reason should pass through, got %q", got)
	}
	long := strings.Repeat("x", 120)
	got := master.ExportTruncateImpersonationReason(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long reason should be truncated with ellipsis, got %q", got)
	}
	if len(got) != 80+len("…") {
		t.Fatalf("expected truncated length to be 80+ellipsis, got %d", len(got))
	}
}

func TestBuildImpersonationContext_NilHandler(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
	if got := master.BuildImpersonationContext(r, nil, "tk", nil); got != nil {
		t.Fatalf("nil session → expected nil context, got %+v", got)
	}
}

// ----- BuildImpersonationContext fallbacks ------------------------------

func TestBuildImpersonationContext_ReturnsNilWhenNoEnvelope(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
	got := master.BuildImpersonationContext(r, &stubTenantsByID{}, "tk", func() time.Time { return time.Now() })
	if got != nil {
		t.Fatalf("expected nil context when no envelope, got %+v", got)
	}
}

func TestBuildImpersonationContext_FallsBackOnResolverError(t *testing.T) {
	t.Parallel()
	ctx, _ := bannerCtx(t, "Acme", "acme")
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil).WithContext(ctx)
	got := master.BuildImpersonationContext(r, &stubTenantsByID{err: errors.New("db boom")}, "tk", func() time.Time { return time.Now() })
	if got == nil {
		t.Fatal("expected non-nil context (banner must remain visible on resolver error)")
	}
	if !strings.HasPrefix(got.TenantName, "tenant:") {
		t.Fatalf("expected hex-fallback tenant name, got %q", got.TenantName)
	}
}

// ----- mini-stubs and helpers --------------------------------------------

type stubGrantRequests struct {
	get             master.GrantRequest
	getAfterApprove master.GrantRequest
	approvedCount   int
	rejectedCount   int
}

func (s *stubGrantRequests) CreateGrantRequest(_ context.Context, _ master.CreateGrantRequestInput) (master.GrantRequest, error) {
	return master.GrantRequest{}, nil
}
func (s *stubGrantRequests) GetGrantRequest(_ context.Context, _ uuid.UUID) (master.GrantRequest, error) {
	if s.approvedCount > 0 {
		out := s.getAfterApprove
		if out.ID == uuid.Nil {
			out = s.get
			out.State = master.GrantRequestStateApproved
		}
		return out, nil
	}
	return s.get, nil
}
func (s *stubGrantRequests) ListAwaitingRequests(_ context.Context) ([]master.GrantRequest, error) {
	return []master.GrantRequest{s.get}, nil
}
func (s *stubGrantRequests) ApproveGrantRequest(_ context.Context, _ master.DecideGrantRequestInput) (master.GrantRow, error) {
	s.approvedCount++
	return master.GrantRow{}, nil
}
func (s *stubGrantRequests) RejectGrantRequest(_ context.Context, _ master.DecideGrantRequestInput) error {
	s.rejectedCount++
	return nil
}

func newHandlerWithGrantRequests(t *testing.T, store master.GrantRequestPort) *master.Handler {
	t.Helper()
	deps := master.Deps{
		Tenants:       &stubLister{res: master.ListResult{Page: 1, PageSize: 25}},
		Creator:       &stubCreator{},
		Plans:         &stubPlans{},
		Assigner:      &stubAssigner{},
		CSRFToken:     func(*http.Request) string { return "csrf-test-token" },
		Logger:        discardSlog(),
		GrantRequests: store,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	return h
}

func makeTenantsPageData() interface{} {
	return master.ExportNewTenantsListData([]master.TenantRow{}, &shell.ImpersonationContext{
		TenantName: "Acme",
		TenantSlug: "acme",
		Reason:     "ticket #11324",
		ExpiresAt:  time.Date(2026, 5, 31, 13, 15, 0, 0, time.UTC),
		ServerNow:  time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
	})
}

func makeTenantsPageDataNoBanner() interface{} {
	return master.ExportNewTenantsListData([]master.TenantRow{}, nil)
}

func makeTenantDetailData() interface{} {
	return master.ExportNewTenantDetailData(master.TenantRow{
		ID:   uuid.New(),
		Name: "Acme Saúde Ltda",
		Host: "acme.crm.local",
	}, false, &shell.ImpersonationContext{
		TenantName: "Acme Saúde Ltda",
		TenantSlug: "acme.crm.local",
		Reason:     "ticket #11324",
		ExpiresAt:  time.Date(2026, 5, 31, 13, 15, 0, 0, time.UTC),
		ServerNow:  time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
	})
}

func renderPage(t *testing.T, page string, data interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	switch page {
	case "tenants_list":
		if err := master.ExportMasterLayoutTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("execute master layout: %v", err)
		}
	case "tenant_detail":
		if err := master.ExportTenantDetailLayoutTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("execute tenant detail layout: %v", err)
		}
	default:
		t.Fatalf("unknown page %q", page)
	}
	return buf.String()
}

func renderTenantsListWithBanner(t *testing.T, bctx *shell.ImpersonationContext) string {
	t.Helper()
	return renderPage(t, "tenants_list", master.ExportNewTenantsListData([]master.TenantRow{}, bctx))
}

func renderGrantRequestDetailPanel(t *testing.T, req master.GrantRequest, viewer uuid.UUID, stage string) string {
	t.Helper()
	data := master.ExportNewGrantRequestDetailData(req, viewer, stage, nil)
	var buf bytes.Buffer
	if err := master.ExportGrantRequestDetailPanelTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("execute grant request detail panel: %v", err)
	}
	return buf.String()
}

func excerpt(haystack, needle string) string {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return "(not found)"
	}
	end := i + len(needle) + 60
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[i:end]
}

func readMasterCSS(t *testing.T) string {
	t.Helper()
	const path = "../../../web/static/css/master.css"
	body, err := readFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// silence audit/impersonation imports when not directly referenced.
var _ = audit.SecurityEventImpersonationStart
var _ = impersonation.DefaultEnvelopeTTL
