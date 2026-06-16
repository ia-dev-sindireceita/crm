package httpapi_test

// SIN-64985 — Deps.WebSurfaces is the single source of truth for the
// `deps.WebX != nil` mount predicates, surfaced on /health (booleans
// only) so an operator can diagnose a silently-nil web surface without
// container-log access. These tests fix the acceptance criterion: a nil
// handler maps to false, a present handler maps to true, and the map
// reaches /health through the router unchanged.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// stubHandler is a non-nil http.Handler used to mark a surface "mounted"
// in Deps without standing up its real wireup.
var stubHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

// allSurfaceKeys is every key WebSurfaces must report. Locking the set
// guards against a new nil-dep-gated surface being added to the router
// without being reflected on /health (the drift this guardrail exists to
// prevent).
var allSurfaceKeys = []string{
	"ai_policy", "catalog", "funnel", "funnel_rules",
	"privacy", "campaigns", "consent", "inbox", "contacts",
}

func TestDeps_WebSurfaces_KeySet(t *testing.T) {
	t.Parallel()
	got := httpapi.Deps{}.WebSurfaces()
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string(nil), allSurfaceKeys...)
	sort.Strings(want)
	if len(keys) != len(want) {
		t.Fatalf("keys=%v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys=%v, want %v", keys, want)
		}
	}
}

func TestDeps_WebSurfaces_NilHandlersAllFalse(t *testing.T) {
	t.Parallel()
	got := httpapi.Deps{}.WebSurfaces()
	for k, v := range got {
		if v {
			t.Fatalf("surfaces[%q]=true, want false for zero-value Deps", k)
		}
	}
}

func TestDeps_WebSurfaces_PresentHandlersTrue(t *testing.T) {
	t.Parallel()
	// Wire every gated surface; each must report true.
	deps := httpapi.Deps{
		WebAIPolicy:    stubHandler,
		WebCatalog:     stubHandler,
		WebFunnel:      stubHandler,
		WebFunnelRules: stubHandler,
		WebPrivacy:     stubHandler,
		WebCampaigns:   stubHandler,
		WebConsent:     stubHandler,
		WebInbox:       stubHandler,
		WebContacts:    stubHandler,
	}
	for k, v := range deps.WebSurfaces() {
		if !v {
			t.Fatalf("surfaces[%q]=false, want true when handler wired", k)
		}
	}
}

// TestDeps_WebSurfaces_MixedReflectsEachPredicate proves each key tracks
// its OWN handler slot — a present surface stays true while its
// neighbours stay false. This is the diagnostic's whole value: pinpoint
// the one surface that failed to wire.
func TestDeps_WebSurfaces_MixedReflectsEachPredicate(t *testing.T) {
	t.Parallel()
	deps := httpapi.Deps{
		WebInbox:    stubHandler,
		WebContacts: stubHandler,
		// ai_policy intentionally nil — simulates a silently-nil surface.
	}
	got := deps.WebSurfaces()
	if !got["inbox"] || !got["contacts"] {
		t.Fatalf("wired surfaces must be true: %v", got)
	}
	if got["ai_policy"] {
		t.Fatalf("ai_policy must be false when its handler is nil: %v", got)
	}
}

// TestRouter_Health_ReportsSurfaces is the end-to-end check: the map
// reaches /health through NewRouter, with booleans matching the wired
// Deps. /health bypasses tenant scope, so any host works.
func TestRouter_Health_ReportsSurfaces(t *testing.T) {
	t.Parallel()
	// NewRouter requires IAM + TenantResolver; /health itself bypasses
	// tenant scope, so a minimal store/resolver is enough.
	acmeID := uuid.New()
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}}
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(map[string]uuid.UUID{"acme.crm.local": acmeID}),
		TenantResolver: resolver,
		WebInbox:       stubHandler,
		// every other gated surface nil → false
	})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Host = "totally-unknown-host.example"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body struct {
		Surfaces map[string]bool `json:"surfaces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Surfaces) != len(allSurfaceKeys) {
		t.Fatalf("surfaces=%v, want %d keys", body.Surfaces, len(allSurfaceKeys))
	}
	if !body.Surfaces["inbox"] {
		t.Fatalf("surfaces[inbox]=false, want true (handler wired)")
	}
	if body.Surfaces["ai_policy"] {
		t.Fatalf("surfaces[ai_policy]=true, want false (handler nil)")
	}
}
