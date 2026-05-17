package aipanel_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/aipanel"
)

// fakeConsent captures the most recent RecordConsent call so the tests
// can assert the handler is forwarding (scope, actor, preview,
// versions) verbatim.
type fakeConsent struct {
	mu      sync.Mutex
	scope   aipolicy.ConsentScope
	actor   *uuid.UUID
	preview string
	anonVer string
	promVer string
	called  bool
	err     error
}

func (f *fakeConsent) RecordConsent(
	_ context.Context,
	scope aipolicy.ConsentScope,
	actor *uuid.UUID,
	payloadPreview, anonymizerVersion, promptVersion string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.scope = scope
	f.actor = actor
	f.preview = payloadPreview
	f.anonVer = anonymizerVersion
	f.promVer = promptVersion
	return f.err
}

func newTestHandler(t *testing.T, consent aipanel.ConsentRecorder, userID uuid.UUID, metrics *obs.Metrics) *aipanel.Handler {
	t.Helper()
	h, err := aipanel.New(aipanel.Deps{
		Consent: consent,
		UserID:  func(*http.Request) uuid.UUID { return userID },
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("aipanel.New: %v", err)
	}
	return h
}

func tenantRequest(method, target, body string, tenantID uuid.UUID) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tenant := &tenancy.Tenant{ID: tenantID}
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func acceptBody(scopeKind, scopeID, anonVer, promVer, preview, conversationID string) string {
	hash := sha256.Sum256([]byte(preview))
	values := []string{
		"scope_kind=" + scopeKind,
		"scope_id=" + scopeID,
		"anonymizer_version=" + anonVer,
		"prompt_version=" + promVer,
		"payload_hash=" + hex.EncodeToString(hash[:]),
		"payload_preview=" + preview,
	}
	if conversationID != "" {
		values = append(values, "conversation_id="+conversationID)
	}
	return strings.Join(values, "&")
}

func TestNew_RequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := aipanel.New(aipanel.Deps{UserID: func(*http.Request) uuid.UUID { return uuid.Nil }}); err == nil {
		t.Fatal("expected error when Consent is missing")
	}
	if _, err := aipanel.New(aipanel.Deps{Consent: &fakeConsent{}}); err == nil {
		t.Fatal("expected error when UserID is missing")
	}
	// Sanity: both supplied → no error.
	if _, err := aipanel.New(aipanel.Deps{
		Consent: &fakeConsent{},
		UserID:  func(*http.Request) uuid.UUID { return uuid.Nil },
	}); err != nil {
		t.Fatalf("New(full): %v", err)
	}
}

// TestAccept_HappyPath_TenantScope drives AC #1 of SIN-62352 from the
// handler's side: the operator confirms, the consent is recorded under
// the (tenant, scope_kind, scope_id) triple, the metric ticks, and the
// response carries the HX-Trigger that re-fires the original request.
func TestAccept_HappyPath_TenantScope(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	userID := uuid.New()
	conversationID := uuid.New().String()
	preview := "Olá *****, seu pedido foi atualizado."
	consent := &fakeConsent{}
	metrics := obs.NewMetrics()

	h := newTestHandler(t, consent, userID, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := acceptBody("tenant", tenantID.String(), "v1", "p2", preview, conversationID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !consent.called {
		t.Fatalf("ConsentService.RecordConsent not called")
	}
	if consent.scope.TenantID != tenantID {
		t.Errorf("scope.TenantID = %s, want %s", consent.scope.TenantID, tenantID)
	}
	if consent.scope.Kind != aipolicy.ScopeTenant {
		t.Errorf("scope.Kind = %q, want tenant", consent.scope.Kind)
	}
	if consent.scope.ID != tenantID.String() {
		t.Errorf("scope.ID = %q, want %q", consent.scope.ID, tenantID.String())
	}
	if consent.actor == nil || *consent.actor != userID {
		t.Errorf("actor = %v, want %v", consent.actor, userID)
	}
	if consent.preview != preview {
		t.Errorf("preview = %q, want %q", consent.preview, preview)
	}
	if consent.anonVer != "v1" || consent.promVer != "p2" {
		t.Errorf("versions = (%q,%q), want (v1,p2)", consent.anonVer, consent.promVer)
	}
	if got := testutil.ToFloat64(metrics.AIConsentTotal.WithLabelValues("tenant", "accepted")); got != 1 {
		t.Errorf("metric ai_consent_total{tenant,accepted} = %v, want 1", got)
	}
	hxTrigger := rec.Header().Get("HX-Trigger")
	if !strings.Contains(hxTrigger, "ai-consent-accepted") {
		t.Errorf("HX-Trigger = %q, want it to contain ai-consent-accepted", hxTrigger)
	}
	if !strings.Contains(hxTrigger, conversationID) {
		t.Errorf("HX-Trigger = %q, want it to contain conversation_id %s", hxTrigger, conversationID)
	}
}

// TestAccept_ActorNilWhenSessionMissing exercises the nullable
// actor_user_id branch: when UserID returns uuid.Nil the row records
// actor as NULL (the migration 0101 ON DELETE SET NULL contract).
func TestAccept_ActorNilWhenSessionMissing(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	consent := &fakeConsent{}
	h := newTestHandler(t, consent, uuid.Nil, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := acceptBody("channel", "whatsapp", "v1", "p1", "preview", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !consent.called {
		t.Fatal("RecordConsent not called")
	}
	if consent.actor != nil {
		t.Errorf("actor = %v, want nil", consent.actor)
	}
}

// TestAccept_HashMismatch_RejectsTampering covers AC #3: the handler
// recomputes SHA-256 over the supplied preview and rejects when the
// body's payload_hash does not match. Without this check a client
// could pre-compute the digest of payload P, then submit payload P'
// and consent on a preview the operator never saw.
func TestAccept_HashMismatch_RejectsTampering(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	consent := &fakeConsent{}
	h := newTestHandler(t, consent, uuid.Nil, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	// Hash of "original" but preview is "tampered" — mismatch must reject.
	hash := sha256.Sum256([]byte("original"))
	body := strings.Join([]string{
		"scope_kind=tenant",
		"scope_id=" + tenantID.String(),
		"anonymizer_version=v1",
		"prompt_version=p1",
		"payload_hash=" + hex.EncodeToString(hash[:]),
		"payload_preview=tampered",
	}, "&")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%q", rec.Code, rec.Body.String())
	}
	if consent.called {
		t.Fatal("RecordConsent must not be called when hash mismatches")
	}
}

// TestAccept_RejectsInvalidScopeKind closes the only enum boundary the
// handler enforces: the service further validates non-empty tenant +
// scope id, but a bad scope_kind must fail at the boundary so the SQL
// layer never sees a CHECK violation.
func TestAccept_RejectsInvalidScopeKind(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	consent := &fakeConsent{}
	h := newTestHandler(t, consent, uuid.Nil, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := acceptBody("nonsense", "x", "v1", "p1", "preview", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if consent.called {
		t.Fatal("RecordConsent must not be called for invalid scope_kind")
	}
}

func TestAccept_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	cases := map[string]string{
		"missing scope_id":           "scope_kind=tenant&anonymizer_version=v1&prompt_version=p1&payload_hash=abc&payload_preview=x",
		"missing anonymizer_version": "scope_kind=tenant&scope_id=x&prompt_version=p1&payload_hash=abc&payload_preview=x",
		"missing prompt_version":     "scope_kind=tenant&scope_id=x&anonymizer_version=v1&payload_hash=abc&payload_preview=x",
		"missing payload_hash":       "scope_kind=tenant&scope_id=x&anonymizer_version=v1&prompt_version=p1&payload_preview=x",
		"missing payload_preview":    "scope_kind=tenant&scope_id=x&anonymizer_version=v1&prompt_version=p1&payload_hash=abc",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			consent := &fakeConsent{}
			h := newTestHandler(t, consent, uuid.Nil, nil)
			mux := http.NewServeMux()
			h.Routes(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400; body=%q", rec.Code, rec.Body.String())
			}
			if consent.called {
				t.Fatal("RecordConsent must not be called for malformed bodies")
			}
		})
	}
}

// TestAccept_ServiceError_Returns500 keeps the error path quiet: the
// downstream RecordConsent failure surfaces as a generic 500 without
// leaking the underlying error into the response body.
func TestAccept_ServiceError_Returns500(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	consent := &fakeConsent{err: errors.New("boom")}
	h := newTestHandler(t, consent, uuid.New(), nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := acceptBody("tenant", tenantID.String(), "v1", "p1", "preview", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.AcceptRoutePath, body, tenantID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("response body leaked downstream error text")
	}
}

func TestAccept_TenantMissingContext_Returns500(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{}
	h := newTestHandler(t, consent, uuid.Nil, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	// Build a request WITHOUT tenancy.WithContext.
	r := httptest.NewRequest(http.MethodPost, aipanel.AcceptRoutePath,
		strings.NewReader(acceptBody("tenant", "x", "v1", "p1", "preview", "")))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if consent.called {
		t.Fatal("RecordConsent must not be called without tenant context")
	}
}

// TestCancel_IncrementsMetricAndRendersPlaceholder covers AC #2: the
// cancel path records the outcome metric and returns the empty modal
// placeholder so HTMX's outerHTML swap collapses the dialog. The
// service is NEVER called on cancel — the test asserts that explicitly
// by passing a fake that fails on any call.
func TestCancel_IncrementsMetricAndRendersPlaceholder(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{err: errors.New("must not be called")}
	metrics := obs.NewMetrics()
	h := newTestHandler(t, consent, uuid.New(), metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.CancelRoutePath, "scope_kind=channel", uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if consent.called {
		t.Fatal("RecordConsent must not be called on cancel")
	}
	if got := testutil.ToFloat64(metrics.AIConsentTotal.WithLabelValues("channel", "cancelled")); got != 1 {
		t.Errorf("metric ai_consent_total{channel,cancelled} = %v, want 1", got)
	}
	if !strings.Contains(rec.Body.String(), `id="ai-consent-modal"`) {
		t.Errorf("body missing #ai-consent-modal placeholder; got %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

// TestCancel_InvalidScopeKind_DropsMetric defends the metric registry
// against a tampered body inflating cardinality: the handler logs and
// drops the metric but still returns the placeholder so the operator's
// UI does not hang.
func TestCancel_InvalidScopeKind_DropsMetric(t *testing.T) {
	t.Parallel()
	metrics := obs.NewMetrics()
	h := newTestHandler(t, &fakeConsent{}, uuid.Nil, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, tenantRequest(http.MethodPost, aipanel.CancelRoutePath, "scope_kind=nonsense", uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	// Neither accepted nor cancelled with the bogus label should have
	// been emitted — assert by gathering and looking for zero series.
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "ai_consent_total" {
			continue
		}
		for _, c := range mf.GetMetric() {
			t.Errorf("ai_consent_total unexpectedly emitted labels=%v", c.GetLabel())
		}
	}
}

// TestRenderConsentModal_EscapesPayload covers the F29 mitigation
// (SIN-62225): the anonymized payload preview flows through the
// html/template auto-escape only, never as template.HTML, so even a
// malicious "<script>" preview becomes inert text inside the <pre>.
func TestRenderConsentModal_EscapesPayload(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	err := aipanel.RenderConsentModal(&buf, aipanel.ConsentModalData{
		ScopeKind:         "tenant",
		ScopeID:           "00000000-0000-0000-0000-000000000001",
		Payload:           `<script>alert("xss")</script>`,
		AnonymizerVersion: "v1",
		PromptVersion:     "p1",
		PayloadHashHex:    "deadbeef0000000000000000000000000000000000000000000000000000",
	})
	if err != nil {
		t.Fatalf("RenderConsentModal: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `<script>alert("xss")</script>`) {
		t.Errorf("payload rendered un-escaped: %s", out)
	}
	if !strings.Contains(out, `&lt;script&gt;`) {
		t.Errorf("expected escaped script tag in output; got %s", out)
	}
	// Sanity: accessibility markers and short hash present.
	for _, needle := range []string{
		`role="dialog"`,
		`aria-modal="true"`,
		`aria-labelledby="ai-consent-heading"`,
		`deadbeef0000`, // first 12 chars of the SHA-256 hex
		`hx-post="/aipanel/consent/accept"`,
		`hx-post="/aipanel/consent/cancel"`,
		`autofocus`,
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("modal missing %q", needle)
		}
	}
	// And the F29 invariant: the full payload hex MUST be present as
	// the hidden hash input so the accept handler can boundary-check it.
	if !strings.Contains(out, `name="payload_hash"`) {
		t.Errorf("modal missing payload_hash hidden input")
	}
}

func TestRenderConsentModal_ShortPayloadStillRenders(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	err := aipanel.RenderConsentModal(&buf, aipanel.ConsentModalData{
		ScopeKind:         "team",
		ScopeID:           "team-A",
		Payload:           "ok",
		AnonymizerVersion: "v1",
		PromptVersion:     "p1",
		// Short hex (5 chars). shortHash should fall back to the
		// whole string rather than panic on the slice.
		PayloadHashHex: "abcde",
	})
	if err != nil {
		t.Fatalf("RenderConsentModal: %v", err)
	}
	if !strings.Contains(buf.String(), "abcde") {
		t.Errorf("short hex not rendered")
	}
}
