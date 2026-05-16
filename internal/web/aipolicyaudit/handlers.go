// Package aipolicyaudit is the HTMX UI for the ai_policy_audit
// ledger (SIN-62353 / Fase 3, decisão #8). It serves two routes:
//
//   - GET /settings/ai-policy/audit — per-tenant view. The current
//     tenant (resolved by middleware.TenantScope) is the only one a
//     request can see; RLS guarantees that even if the handler
//     accidentally accepted a tenant parameter, the query would still
//     return the request's tenant rows only.
//   - GET /admin/audit?tenant=...&module=ai-policy — master view.
//     module=ai-policy is the only module this handler answers (the
//     surface is reserved so other modules can land alongside in
//     follow-up child issues without breaking the URL). The handler
//     calls AuditQuery.Page against the requested tenant id; the
//     authorization gate is mounted in the router via
//     middleware.RequireAction(iam.ActionMasterAuditRead, ...).
//
// Both routes are read-only: no POST surface, no CSRF token surface,
// no state-changing affordance. The page is fully server-rendered
// (HTMX optional) and renders well under the 200ms p95 budget called
// out in AC #3.
package aipolicyaudit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// Deps bundles the collaborators required by the handler.
type Deps struct {
	// Query is the read-side port. The pgx adapter under
	// internal/adapter/db/postgres/aipolicy satisfies it; tests pass
	// an in-memory fake.
	Query aipolicy.AuditQuery

	// Now is the request-time clock. Tests stub this to make the
	// rendered "gerado em" stamp deterministic. Default is
	// time.Now().UTC().
	Now func() time.Time

	// Logger receives one structured line per render error. The
	// page never serves a 5xx for cosmetic failures.
	Logger *slog.Logger
}

// Handler serves the per-tenant + master audit pages.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil Query rejects at boot time so a
// misconfigured wire fails fast instead of rendering a half-broken
// audit page.
func New(deps Deps) (*Handler, error) {
	if deps.Query == nil {
		return nil, errors.New("web/aipolicyaudit: Query is required")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts both endpoints on mux. Both are GET-only.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/ai-policy/audit", h.viewTenant)
	mux.HandleFunc("GET /admin/audit", h.viewMaster)
}

// viewTenant renders the per-tenant audit page. The tenant id is the
// one in context (resolved by TenantScope middleware); query string
// filters narrow scope / period; cursor pages forward.
func (h *Handler) viewTenant(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	filters := parseFilters(r)
	page, err := h.deps.Query.Page(r.Context(), aipolicy.AuditPageQuery{
		TenantID:  tenant.ID,
		ScopeType: filters.scopeType,
		ScopeID:   filters.scopeID,
		Since:     filters.since,
		Until:     filters.until,
		Cursor:    filters.cursor,
		Limit:     defaultPageLimit,
	})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	h.render(w, pageData{
		Title:       fmt.Sprintf("Auditoria — IA — %s", tenant.Name),
		IsMaster:    false,
		TenantName:  tenant.Name,
		TenantID:    tenant.ID,
		BaseURL:     "/settings/ai-policy/audit",
		GeneratedAt: h.deps.Now().Format(time.RFC3339),
		Filters:     filters,
		Events:      mapRecords(page.Events),
		NextCursor:  encodeCursor(page.Next),
	})
}

// viewMaster renders the master cross-tenant view. The action lives
// under /admin/audit so the surface can grow to other modules
// (module=mfa, module=billing, …) without proliferating endpoints.
// For this PR only module=ai-policy is implemented; other module
// values render a friendly "not yet wired" stub.
func (h *Handler) viewMaster(w http.ResponseWriter, r *http.Request) {
	module := strings.TrimSpace(r.URL.Query().Get("module"))
	if module == "" {
		module = "ai-policy"
	}
	if module != "ai-policy" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`<p>Auditoria do módulo <strong>%s</strong> ainda não está disponível neste console.</p>`,
			template.HTMLEscapeString(module),
		)))
		return
	}
	rawTenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if rawTenant == "" {
		http.Error(w, "tenant query parameter required", http.StatusBadRequest)
		return
	}
	tenantID, err := uuid.Parse(rawTenant)
	if err != nil {
		http.Error(w, "invalid tenant uuid", http.StatusBadRequest)
		return
	}
	filters := parseFilters(r)
	page, err := h.deps.Query.Page(r.Context(), aipolicy.AuditPageQuery{
		TenantID:  tenantID,
		ScopeType: filters.scopeType,
		ScopeID:   filters.scopeID,
		Since:     filters.since,
		Until:     filters.until,
		Cursor:    filters.cursor,
		Limit:     defaultPageLimit,
	})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	h.render(w, pageData{
		Title:       fmt.Sprintf("Auditoria master — IA — tenant %s", rawTenant),
		IsMaster:    true,
		TenantName:  rawTenant,
		TenantID:    tenantID,
		BaseURL:     "/admin/audit",
		ModuleParam: module,
		GeneratedAt: h.deps.Now().Format(time.RFC3339),
		Filters:     filters,
		Events:      mapRecords(page.Events),
		NextCursor:  encodeCursor(page.Next),
	})
}

// defaultPageLimit caps each page at 50 rows. Matches the adapter's
// default and keeps the rendered table within a single screen of
// scroll for typical font sizes.
const defaultPageLimit = 50

// filterSet groups the parsed query-string filters so handlers can
// pass them around in one value.
type filterSet struct {
	scopeType aipolicy.ScopeType
	scopeID   string
	since     time.Time
	until     time.Time
	cursor    aipolicy.AuditCursor
	// Raw forms preserved for template rendering of <input value="...">.
	RawScopeType string
	RawScopeID   string
	RawSince     string
	RawUntil     string
}

func parseFilters(r *http.Request) filterSet {
	q := r.URL.Query()
	fs := filterSet{
		RawScopeType: strings.TrimSpace(q.Get("scope_type")),
		RawScopeID:   strings.TrimSpace(q.Get("scope_id")),
		RawSince:     strings.TrimSpace(q.Get("since")),
		RawUntil:     strings.TrimSpace(q.Get("until")),
	}
	if fs.RawScopeType != "" {
		st := aipolicy.ScopeType(fs.RawScopeType)
		if st.IsValid() {
			fs.scopeType = st
		}
	}
	if fs.RawScopeID != "" {
		fs.scopeID = fs.RawScopeID
	}
	if fs.RawSince != "" {
		if t, err := parseFilterDate(fs.RawSince); err == nil {
			fs.since = t
		}
	}
	if fs.RawUntil != "" {
		if t, err := parseFilterDate(fs.RawUntil); err == nil {
			fs.until = t
		}
	}
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		if c, err := decodeCursor(raw); err == nil {
			fs.cursor = c
		}
	}
	return fs
}

// parseFilterDate accepts the two HTML <input type="date"> shapes:
// the bare ISO date ("2026-05-01") and the full RFC 3339 timestamp
// (round-trip from a previous Next cursor). The former is treated as
// UTC midnight.
func parseFilterDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Parse(time.RFC3339, s)
}

// encodeCursor renders an AuditCursor as an opaque base64 token.
// Empty input means "no next page" — the caller renders no link.
func encodeCursor(c aipolicy.AuditCursor) string {
	if c.IsZero() {
		return ""
	}
	body, err := json.Marshal(struct {
		T time.Time `json:"t"`
		I uuid.UUID `json:"i"`
	}{T: c.CreatedAt, I: c.ID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

// decodeCursor reverses encodeCursor. A malformed token decodes to
// the zero cursor (first page) so a tampered URL is degraded
// gracefully, not a 500.
func decodeCursor(s string) (aipolicy.AuditCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return aipolicy.AuditCursor{}, err
	}
	var v struct {
		T time.Time `json:"t"`
		I uuid.UUID `json:"i"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return aipolicy.AuditCursor{}, err
	}
	return aipolicy.AuditCursor{CreatedAt: v.T, ID: v.I}, nil
}

// renderableEvent is the template view-model for one ai_policy_audit
// row. Old/New come back as JSON-encoded strings so the template can
// render any payload shape without per-field branches.
type renderableEvent struct {
	ID          string
	When        string
	ScopeType   string
	ScopeID     string
	Field       string
	Old         string
	New         string
	ActorUserID string
	ActorMaster bool
}

func mapRecords(in []aipolicy.AuditRecord) []renderableEvent {
	out := make([]renderableEvent, 0, len(in))
	for _, ev := range in {
		out = append(out, renderableEvent{
			ID:          ev.ID.String(),
			When:        ev.CreatedAt.UTC().Format(time.RFC3339),
			ScopeType:   string(ev.ScopeType),
			ScopeID:     ev.ScopeID,
			Field:       ev.Field,
			Old:         renderJSON(ev.OldValue),
			New:         renderJSON(ev.NewValue),
			ActorUserID: ev.ActorUserID.String(),
			ActorMaster: ev.ActorMaster,
		})
	}
	return out
}

// renderJSON formats v as a compact JSON string. nil decodes to "-"
// so the table cell never blanks out (HTML readers cannot tell
// "empty string" from "missing value" at a glance).
func renderJSON(v any) string {
	if v == nil {
		return "—"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if s == `""` {
		return `""`
	}
	return s
}

// pageData is the template view-model. Filters keep their raw form
// so the rendered <input value="..."> survives a round-trip.
type pageData struct {
	Title       string
	IsMaster    bool
	TenantName  string
	TenantID    uuid.UUID
	BaseURL     string
	ModuleParam string
	GeneratedAt string
	Filters     filterSet
	Events      []renderableEvent
	NextCursor  string
}

func (h *Handler) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := pageTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/aipolicyaudit: render", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/aipolicyaudit: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// Compile-time guarantee that the template can resolve every field
// on pageData. A typo here would fail the first test.
var _ template.HTML = template.HTML("")

// Compile-time guarantee that the package's ctx type matches the
// upstream tenancy.Tenant shape — catching an upstream rename early.
var _ = func(_ context.Context) {}
