package handler_test

// SIN-64985 — tests for the WithSurfaces option on handler.Health. The
// /health surfaces map lets an operator diagnose a silently-nil web
// surface (router skips the `deps.WebX != nil` mount → bare 404) with a
// single curl, no container-log access. The security contract is that
// ONLY booleans cross the boundary — never the wire failure reason — so
// the JSON shape and the value type are both load-bearing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
)

const surfacesTestSHA = "0123456789abcdef0123456789abcdef01234567"

// decodeSurfaces serves /health and decodes the surfaces sub-object.
// found reports whether the top-level "surfaces" key was present so a
// caller can distinguish "omitted" from "present but empty".
func decodeSurfaces(t *testing.T, h http.HandlerFunc) (surfaces map[string]bool, found bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := body["surfaces"]
	if !ok {
		return nil, false
	}
	if err := json.Unmarshal(raw, &surfaces); err != nil {
		t.Fatalf("decode surfaces: %v", err)
	}
	return surfaces, true
}

// TestHealth_DefaultOmitsSurfaces locks the legacy JSON shape: a caller
// that passes only the SHA must not see the new surfaces field, so the
// LB / oncall tooling that predates SIN-64985 keeps its original shape.
func TestHealth_DefaultOmitsSurfaces(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(surfacesTestSHA).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "surfaces") {
		t.Fatalf("body=%q must omit surfaces when option not set", body)
	}
}

// TestHealth_WithSurfaces_NilOmits ensures a nil map omits the field —
// the cmd/server boot path passes nil when the router has not published
// the map yet (a probe issued before wireup completes). It must NOT
// surface as `"surfaces":null`, which downstream tooling could misread.
func TestHealth_WithSurfaces_NilOmits(t *testing.T) {
	t.Parallel()
	_, found := decodeSurfaces(t, handler.Health(surfacesTestSHA, handler.WithSurfaces(nil)))
	if found {
		t.Fatalf("nil map must omit the surfaces field")
	}
}

// TestHealth_WithSurfaces_EmptyOmits ensures an explicit empty map also
// omits the field — an empty map carries no diagnostic signal and would
// only pollute the legacy shape.
func TestHealth_WithSurfaces_EmptyOmits(t *testing.T) {
	t.Parallel()
	_, found := decodeSurfaces(t, handler.Health(surfacesTestSHA, handler.WithSurfaces(map[string]bool{})))
	if found {
		t.Fatalf("empty map must omit the surfaces field")
	}
}

// TestHealth_WithSurfaces_RendersBooleans is the core diagnostic: a
// populated map renders verbatim, INCLUDING false values (a false entry
// is the silently-nil-surface signal — dropping it would defeat the
// whole feature). It also confirms the surfaces field coexists with the
// pre-existing status / commit_sha fields.
func TestHealth_WithSurfaces_RendersBooleans(t *testing.T) {
	t.Parallel()
	in := map[string]bool{"ai_policy": false, "inbox": true, "contacts": true}
	rec := httptest.NewRecorder()
	handler.Health(surfacesTestSHA, handler.WithSurfaces(in)).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body struct {
		Status    string          `json:"status"`
		CommitSHA string          `json:"commit_sha"`
		Surfaces  map[string]bool `json:"surfaces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status=%q, want ok", body.Status)
	}
	if body.CommitSHA != surfacesTestSHA {
		t.Fatalf("commit_sha=%q, want %q", body.CommitSHA, surfacesTestSHA)
	}
	if len(body.Surfaces) != len(in) {
		t.Fatalf("surfaces len=%d, want %d (%v)", len(body.Surfaces), len(in), body.Surfaces)
	}
	for k, want := range in {
		if got, ok := body.Surfaces[k]; !ok || got != want {
			t.Fatalf("surfaces[%q]=%v (present=%v), want %v", k, got, ok, want)
		}
	}
}

// TestHealth_WithSurfaces_OnlyBooleans is the security regression guard:
// the serialized surfaces values must be JSON booleans, never strings.
// A string here would mean a wire failure reason (DSN / infra detail)
// could leak onto the unauthenticated /health endpoint — explicitly
// forbidden by the SIN-64985 security bar.
func TestHealth_WithSurfaces_OnlyBooleans(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(surfacesTestSHA, handler.WithSurfaces(map[string]bool{"inbox": true, "funnel": false})).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	var body struct {
		Surfaces map[string]json.RawMessage `json:"surfaces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, raw := range body.Surfaces {
		s := strings.TrimSpace(string(raw))
		if s != "true" && s != "false" {
			t.Fatalf("surfaces[%q]=%s is not a bare JSON boolean (leak risk)", k, s)
		}
	}
}

// TestHealth_WithSurfaces_DefensiveCopy proves the option snapshots the
// caller's map: mutating it after Health is constructed must not change
// the rendered response. Deps.WebSurfaces() returns a fresh map today,
// but the contract guards a future caller that reuses a shared map.
func TestHealth_WithSurfaces_DefensiveCopy(t *testing.T) {
	t.Parallel()
	in := map[string]bool{"inbox": true}
	h := handler.Health(surfacesTestSHA, handler.WithSurfaces(in))
	in["inbox"] = false   // mutate after construction
	in["contacts"] = true // add after construction
	surfaces, found := decodeSurfaces(t, h)
	if !found {
		t.Fatalf("surfaces field must be present")
	}
	if got := surfaces["inbox"]; got != true {
		t.Fatalf("surfaces[inbox]=%v, want true (mutation must not leak)", got)
	}
	if _, ok := surfaces["contacts"]; ok {
		t.Fatalf("surfaces must not contain post-construction addition 'contacts'")
	}
}

// TestHealth_WithSurfaces_CoexistsWithInboxProvider proves the two
// options compose: cmd/server's production /health passes both
// WithInboxChannelProvider and WithSurfaces on every request.
func TestHealth_WithSurfaces_CoexistsWithInboxProvider(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(
		surfacesTestSHA,
		handler.WithInboxChannelProvider("llmcustomer"),
		handler.WithSurfaces(map[string]bool{"inbox": true}),
	).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body struct {
		InboxChannelProvider string          `json:"inbox_channel_provider"`
		Surfaces             map[string]bool `json:"surfaces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InboxChannelProvider != "llmcustomer" {
		t.Fatalf("inbox_channel_provider=%q, want llmcustomer", body.InboxChannelProvider)
	}
	if body.Surfaces["inbox"] != true {
		t.Fatalf("surfaces[inbox]=%v, want true", body.Surfaces["inbox"])
	}
}
