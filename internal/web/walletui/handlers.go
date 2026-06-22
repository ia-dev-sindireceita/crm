package walletui

// SIN-63942 / UX-F5 — gerente-facing HTMX wallet UI. Three routes share
// one Handler:
//
//	GET /wallet              — dashboard (saldo + projeção + preview)
//	GET /wallet/topup        — catálogo de pacotes
//	GET /wallet/ledger       — ledger paginado + filtros
//	GET /wallet/ledger.csv   — exportação CSV
//
// Authorization is enforced at the router boundary via
// RequireAuth + RequireAction(iam.ActionTenantWalletViewLedger). The
// handler trusts the principal/tenant already on the request context
// and refuses to render without them (fail-closed 500 for missing
// scope is preferred over a "no data" silent render).
//
// LGPD: the rendered ledger rows never expose raw conversation_id or
// message bodies — only the LGPD-redacted projection from the
// walletui.LedgerReader port. WCAG 1.4.1 (color não-único) is honoured
// by pairing the balance severity colour with a textual label and an
// aria-attributed icon, so an operator with monochrome vision sees the
// state from copy + glyph alone.

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// defaultPageSize is the row count the ledger handler asks the adapter
// to return per HTMX request. Kept modest so the first paint stays
// under the Doherty 400ms budget on the slowest target hosts.
const defaultPageSize = 25

// maxPageSize caps user-supplied ?page_size=N values so a stray query
// param can't ask for a 10k-row scan.
const maxPageSize = 200

// csvFilename is the export filename suggested in Content-Disposition.
// The current date is appended at handler time so two consecutive
// exports don't collide in the operator's downloads folder.
const csvFilename = "wallet-ledger"

// CSRFTokenFn returns the request's CSRF token. The empty string is a
// programming error (handlers run behind RequireAuth which guarantees
// the session); the handler surfaces empty as 500 instead of rendering
// a form/meta without a token.
type CSRFTokenFn func(*http.Request) string

// UserDisplayFn returns the visible user-menu label for the principal
// on this request. Empty falls back to "Conta" via shell.
type UserDisplayFn func(*http.Request) string

// NowFn returns the current time. Injectable so tests can pin time
// without mocking the package-level time.Now.
type NowFn func() time.Time

// Deps bundles the handler collaborators. All ports are required so
// cmd/server fails fast at boot rather than serving half-wired routes.
type Deps struct {
	Dashboard DashboardReader
	Ledger    LedgerReader
	Topup     TopupCatalogReader
	CSRFToken CSRFTokenFn
	UserLabel UserDisplayFn
	Now       NowFn
	Logger    *slog.Logger
}

// Handler is the HTMX front controller for the F5 wallet UI.
type Handler struct {
	deps Deps
}

// New wires the Handler. Missing required deps are rejected so
// cmd/server fails at boot.
func New(deps Deps) (*Handler, error) {
	if deps.Dashboard == nil {
		return nil, errors.New("web/walletui: Dashboard is required")
	}
	if deps.Ledger == nil {
		return nil, errors.New("web/walletui: Ledger is required")
	}
	if deps.Topup == nil {
		return nil, errors.New("web/walletui: Topup is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/walletui: CSRFToken is required")
	}
	if deps.UserLabel == nil {
		deps.UserLabel = func(*http.Request) string { return "" }
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the four endpoints on mux. The router wraps this
// mux with RequireAuth + RequireAction before mounting.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /wallet", h.dashboard)
	mux.HandleFunc("GET /wallet/topup", h.topup)
	mux.HandleFunc("GET /wallet/ledger", h.ledger)
	mux.HandleFunc("GET /wallet/ledger.csv", h.ledgerCSV)
}

// ---------------------------------------------------------------------------
// GET /wallet
// ---------------------------------------------------------------------------

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	now := h.deps.Now().UTC()
	snap, err := h.deps.Dashboard.Snapshot(r.Context(), tenant.ID, now)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			snap = DashboardSnapshot{}
		} else {
			h.fail(w, http.StatusInternalServerError, "dashboard snapshot", err)
			return
		}
	}
	data := buildDashboardView(tenant.Name, token, r, snap, now)
	h.cacheable(w)
	h.writeHTML(w, dashboardTmpl, data)
}

// ---------------------------------------------------------------------------
// GET /wallet/topup
// ---------------------------------------------------------------------------

func (h *Handler) topup(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	packages, err := h.deps.Topup.ListPackages(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list topup packages", err)
		return
	}
	data := buildTopupView(tenant.Name, token, r, packages)
	h.cacheable(w)
	h.writeHTML(w, topupTmpl, data)
}

// ---------------------------------------------------------------------------
// GET /wallet/ledger
// ---------------------------------------------------------------------------

func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	opts, filterView, csvURL := parseLedgerQuery(r, tenant.ID)
	page, err := h.deps.Ledger.Page(r.Context(), opts)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			page = LedgerPage{}
		} else {
			h.fail(w, http.StatusInternalServerError, "ledger page", err)
			return
		}
	}
	rowsView := buildRowsView(page, opts.Filter)
	w.Header().Set("Cache-Control", "private, no-store")
	// HTMX swap: only the rows partial.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := ledgerRowsTmpl.ExecuteTemplate(w, "wallet_ledger_rows", rowsView); err != nil {
			h.deps.Logger.Error("web/walletui: render ledger rows", "err", err)
		}
		return
	}
	data := buildLedgerView(tenant.Name, token, r, filterView, csvURL, rowsView)
	h.writeHTML(w, ledgerLayoutTmpl, data)
}

// ---------------------------------------------------------------------------
// GET /wallet/ledger.csv
// ---------------------------------------------------------------------------

func (h *Handler) ledgerCSV(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	filter, _, _ := parseLedgerFilter(r, tenant.ID)
	stamp := h.deps.Now().UTC().Format("2006-01-02")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", csvFilename+"-"+stamp+".csv"))
	if err := h.deps.Ledger.StreamCSV(r.Context(), filter, w); err != nil {
		// Headers are already on the wire by the time the adapter
		// errors mid-stream; nothing useful to surface to the operator
		// — log and end the response cleanly.
		h.deps.Logger.Error("web/walletui: stream csv", "err", err)
	}
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (h *Handler) writeHTML(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.deps.Logger.Error("web/walletui: render", "template", tmpl.Name(), "err", err)
	}
}

// cacheable applies the AC#3 30-second private cache hint. The operator
// hitting the dashboard repeatedly within 30s avoids a fresh DB round
// trip — Cache-Control is honoured by browsers and intermediate proxies
// (we set "private" so shared caches don't store tenant-scoped data).
func (h *Handler) cacheable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age=30")
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/walletui: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// ---------------------------------------------------------------------------
// view shaping — dashboard
// ---------------------------------------------------------------------------

type dashboardView struct {
	// shell.Data field set — read by the shell.Layout reflection helpers.
	TenantName       string
	UserDisplayName  string
	TenantLogo       string
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem

	GeneratedAt string

	Balance    balanceCard
	Projection projectionCard
	Banner     bannerView
	Preview    []ledgerEntryView
}

type balanceCard struct {
	Severity       string // "ok" / "warn" / "critical" / "blocked"
	SeverityLabel  string
	GlyphIcon      string // Pitho icon name (see internal/web/icon); rendered via {{icon}}
	Hint           string
	AvailableLabel string
	BalanceLabel   string
	ReservedLabel  string
	ShowReserved   bool
}

type projectionCard struct {
	AvgLabel       string
	RemainingLabel string
}

type bannerView struct {
	Severity string // "warn" / "outbound" / "full" / "override"
	Title    string
	Message  string
	Visible  bool
}

type ledgerEntryView struct {
	Kind               string
	KindLabel          string
	SourceLabel        string
	OccurredAtLabel    string
	OccurredAtISO      string
	Direction          string // "credit" or "debit"
	AmountLabel        string
	BalanceAfterLabel  string
	ConversationIDHash string
	PolicyIDShort      string
	PolicyID           string
	Model              string
}

func buildDashboardView(tenantName, token string, r *http.Request, snap DashboardSnapshot, now time.Time) dashboardView {
	view := dashboardView{
		TenantName:       tenantName,
		CSRFToken:        token,
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		NavItems:         dashboardNav(),
		UserMenuItems:    userMenu(),
		GeneratedAt:      now.Format(time.RFC3339),
		Balance:          balanceFrom(snap),
		Projection:       projectionFrom(snap),
		Banner:           bannerFrom(snap),
	}
	view.Preview = make([]ledgerEntryView, 0, len(snap.LastFive))
	for _, e := range snap.LastFive {
		view.Preview = append(view.Preview, entryViewFrom(e))
	}
	return view
}

// balanceFrom maps the dashboard snapshot onto the balance card. The
// severity ladder is:
//
//	blocked  → dunning state ∈ {suspended_outbound, suspended_full}
//	critical → available <= 5% of 30-day projection OR available <= 0
//	warn     → available <  20% of 30-day projection
//	ok       → otherwise
//
// When AvgDailyConsume == 0 the projection has no anchor; the card
// falls back to "ok" with a "novo tenant" hint so a freshly-onboarded
// gerente doesn't see a red card before any consumption is recorded.
func balanceFrom(snap DashboardSnapshot) balanceCard {
	card := balanceCard{
		AvailableLabel: humanInt(snap.Available),
		BalanceLabel:   humanInt(snap.Balance),
		ReservedLabel:  humanInt(snap.Reserved),
		ShowReserved:   snap.Reserved > 0 || snap.Balance != snap.Available,
	}
	switch snap.DunningState {
	case "suspended_outbound", "suspended_full":
		card.Severity = "blocked"
		card.SeverityLabel = "Saldo bloqueado"
		card.GlyphIcon = "octagon-alert"
		card.Hint = "Pagamento em atraso bloqueou novas operações até regularização."
		return card
	}
	if snap.AvgDailyConsume <= 0 {
		card.Severity = "ok"
		card.SeverityLabel = "Saldo confortável"
		card.GlyphIcon = "check-circle"
		card.Hint = "Consumo ainda não registrado nos últimos 14 dias."
		return card
	}
	monthly := snap.AvgDailyConsume * 30
	if monthly <= 0 {
		monthly = 1
	}
	ratio := float64(snap.Available) / float64(monthly)
	switch {
	case snap.Available <= 0 || ratio < 0.05:
		card.Severity = "critical"
		card.SeverityLabel = "Saldo crítico"
		card.GlyphIcon = "octagon-alert"
		card.Hint = "Saldo abaixo de 5% do consumo mensal projetado — recarregue para evitar interrupção."
	case ratio < 0.20:
		card.Severity = "warn"
		card.SeverityLabel = "Saldo baixo"
		card.GlyphIcon = "octagon-alert"
		card.Hint = "Saldo abaixo de 20% do consumo mensal projetado — planeje a próxima recarga."
	default:
		card.Severity = "ok"
		card.SeverityLabel = "Saldo confortável"
		card.GlyphIcon = "check-circle"
		card.Hint = "Saldo dentro da margem confortável para o consumo recente."
	}
	return card
}

func projectionFrom(snap DashboardSnapshot) projectionCard {
	card := projectionCard{}
	if snap.AvgDailyConsume <= 0 {
		card.AvgLabel = "—"
		card.RemainingLabel = "Sem consumo recente para projeção."
		return card
	}
	card.AvgLabel = humanInt(snap.AvgDailyConsume)
	if snap.DaysRemaining == nil {
		card.RemainingLabel = "Projeção indisponível."
		return card
	}
	switch d := *snap.DaysRemaining; {
	case d <= 0:
		card.RemainingLabel = "Saldo esgota nas próximas horas no ritmo atual."
	case d == 1:
		card.RemainingLabel = "Esgotamento estimado em 1 dia no ritmo atual."
	default:
		card.RemainingLabel = "Esgotamento estimado em " + strconv.Itoa(d) + " dias no ritmo atual."
	}
	return card
}

func bannerFrom(snap DashboardSnapshot) bannerView {
	if snap.DunningOverrideUntil != nil {
		return bannerView{
			Severity: "override",
			Title:    "Prorrogação concedida",
			Message:  "O master concedeu prorrogação até " + snap.DunningOverrideUntil.UTC().Format("02/01/2006") + ".",
			Visible:  true,
		}
	}
	switch snap.DunningState {
	case "warn":
		return bannerView{
			Severity: "warn",
			Title:    "Fatura em atraso",
			Message:  "Quite a fatura PIX em aberto para evitar suspensão.",
			Visible:  true,
		}
	case "suspended_outbound":
		return bannerView{
			Severity: "outbound",
			Title:    "Envios suspensos",
			Message:  "Envio de mensagens está suspenso. Regularize o pagamento para reativar.",
			Visible:  true,
		}
	case "suspended_full":
		return bannerView{
			Severity: "full",
			Title:    "Conta em modo leitura",
			Message:  "Operações de escrita foram bloqueadas. Regularize o pagamento.",
			Visible:  true,
		}
	}
	return bannerView{}
}

// ---------------------------------------------------------------------------
// view shaping — topup
// ---------------------------------------------------------------------------

type topupView struct {
	TenantName       string
	UserDisplayName  string
	TenantLogo       string
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem

	Packages []topupCard
}

type topupCard struct {
	Slug        string
	Name        string
	TokensLabel string
	PriceLabel  string
	RateLabel   string
	BestValue   bool
}

func buildTopupView(tenantName, token string, r *http.Request, packages []TopupPackage) topupView {
	view := topupView{
		TenantName:       tenantName,
		CSRFToken:        token,
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		NavItems:         topupNav(),
		UserMenuItems:    userMenu(),
	}
	view.Packages = make([]topupCard, 0, len(packages))
	bestIdx := bestValueIndex(packages)
	for i, p := range packages {
		view.Packages = append(view.Packages, topupCard{
			Slug:        p.Slug,
			Name:        p.Name,
			TokensLabel: humanInt(p.Tokens),
			PriceLabel:  formatBRL(p.PriceCentsBRL),
			RateLabel:   formatBRL(p.PricePerKToken),
			BestValue:   i == bestIdx,
		})
	}
	return view
}

// bestValueIndex returns the index of the cheapest cost-per-1k-tokens
// row. Returns -1 when the slice is empty so the template renders no
// badge.
func bestValueIndex(packages []TopupPackage) int {
	if len(packages) == 0 {
		return -1
	}
	best := 0
	for i := 1; i < len(packages); i++ {
		if packages[i].PricePerKToken < packages[best].PricePerKToken {
			best = i
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// view shaping — ledger
// ---------------------------------------------------------------------------

type ledgerView struct {
	TenantName       string
	UserDisplayName  string
	TenantLogo       string
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem

	Filter ledgerFilterView
	CSVURL string
	Rows   rowsView
}

type ledgerFilterView struct {
	From        string
	To          string
	KindOptions []ledgerKindOption
}

type ledgerKindOption struct {
	Value    string
	Label    string
	Selected bool
}

type rowsView struct {
	Entries []ledgerEntryView
	HasMore bool
	NextURL string
}

func buildLedgerView(tenantName, token string, r *http.Request, filter ledgerFilterView, csvURL string, rows rowsView) ledgerView {
	return ledgerView{
		TenantName:       tenantName,
		CSRFToken:        token,
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		NavItems:         ledgerNav(),
		UserMenuItems:    userMenu(),
		Filter:           filter,
		CSVURL:           csvURL,
		Rows:             rows,
	}
}

func buildRowsView(page LedgerPage, filter LedgerFilter) rowsView {
	out := rowsView{HasMore: page.HasMore}
	out.Entries = make([]ledgerEntryView, 0, len(page.Entries))
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, entryViewFrom(e))
	}
	if page.HasMore {
		out.NextURL = buildNextURL(filter, page.NextCursorOccurredAt, page.NextCursorID)
	}
	return out
}

// parseLedgerQuery parses the ledger query string into the adapter
// options + the view model the filter form needs. csvURL is the link
// that mirrors the current filter for the export button.
func parseLedgerQuery(r *http.Request, tenantID uuid.UUID) (LedgerPageOptions, ledgerFilterView, string) {
	filter, filterView, csvURL := parseLedgerFilter(r, tenantID)
	opts := LedgerPageOptions{Filter: filter, PageSize: defaultPageSize}
	if v := strings.TrimSpace(r.URL.Query().Get("page_size")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxPageSize {
				n = maxPageSize
			}
			opts.PageSize = n
		}
	}
	if at := strings.TrimSpace(r.URL.Query().Get("cursor_at")); at != "" {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			opts.CursorOccurredAt = t
		}
	}
	if id := strings.TrimSpace(r.URL.Query().Get("cursor_id")); id != "" {
		if parsed, err := uuid.Parse(id); err == nil {
			opts.CursorID = parsed
		}
	}
	return opts, filterView, csvURL
}

// parseLedgerFilter parses just the shared filter half of the query —
// reused by the ledger page, the rows partial, and the CSV export.
func parseLedgerFilter(r *http.Request, tenantID uuid.UUID) (LedgerFilter, ledgerFilterView, string) {
	q := r.URL.Query()
	from := strings.TrimSpace(q.Get("from"))
	to := strings.TrimSpace(q.Get("to"))
	kind := strings.TrimSpace(q.Get("kind"))

	filter := LedgerFilter{TenantID: tenantID}
	if t, ok := parseDate(from); ok {
		filter.FromOccurredAt = t
	}
	if t, ok := parseDate(to); ok {
		filter.ToOccurredAt = t.Add(24 * time.Hour)
	}
	if kk, ok := parseKind(kind); ok {
		filter.Kinds = []wallet.LedgerKind{kk}
	}
	filterView := ledgerFilterView{
		From:        from,
		To:          to,
		KindOptions: kindOptions(kind),
	}
	csvURL := buildCSVURL(from, to, kind)
	return filter, filterView, csvURL
}

// parseDate accepts YYYY-MM-DD only — the HTML input type=date emits
// that shape. Anything else collapses to zero so the adapter sees "no
// bound".
func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func parseKind(s string) (wallet.LedgerKind, bool) {
	switch s {
	case string(wallet.KindReserve):
		return wallet.KindReserve, true
	case string(wallet.KindCommit):
		return wallet.KindCommit, true
	case string(wallet.KindRelease):
		return wallet.KindRelease, true
	case string(wallet.KindGrant):
		return wallet.KindGrant, true
	default:
		return "", false
	}
}

func kindOptions(selected string) []ledgerKindOption {
	all := []ledgerKindOption{
		{Value: "", Label: "Todos os tipos"},
		{Value: string(wallet.KindCommit), Label: "Consumo confirmado"},
		{Value: string(wallet.KindReserve), Label: "Consumo reservado"},
		{Value: string(wallet.KindRelease), Label: "Reserva liberada"},
		{Value: string(wallet.KindGrant), Label: "Crédito (top-up / cortesia)"},
	}
	for i := range all {
		if all[i].Value == selected {
			all[i].Selected = true
		}
	}
	return all
}

func buildCSVURL(from, to, kind string) string {
	params := []string{}
	if from != "" {
		params = append(params, "from="+from)
	}
	if to != "" {
		params = append(params, "to="+to)
	}
	if kind != "" {
		params = append(params, "kind="+kind)
	}
	if len(params) == 0 {
		return "/wallet/ledger.csv"
	}
	return "/wallet/ledger.csv?" + strings.Join(params, "&")
}

func buildNextURL(filter LedgerFilter, cursorAt time.Time, cursorID uuid.UUID) string {
	params := []string{
		"cursor_at=" + cursorAt.UTC().Format(time.RFC3339Nano),
		"cursor_id=" + cursorID.String(),
	}
	if !filter.FromOccurredAt.IsZero() {
		params = append(params, "from="+filter.FromOccurredAt.UTC().Format("2006-01-02"))
	}
	if !filter.ToOccurredAt.IsZero() {
		// Subtract the 24h we added in parseLedgerFilter so the URL
		// round-trips back to the same input.
		params = append(params, "to="+filter.ToOccurredAt.Add(-24*time.Hour).UTC().Format("2006-01-02"))
	}
	if len(filter.Kinds) == 1 {
		params = append(params, "kind="+string(filter.Kinds[0]))
	}
	return "/wallet/ledger?" + strings.Join(params, "&")
}

// ---------------------------------------------------------------------------
// shared chrome
// ---------------------------------------------------------------------------

func dashboardNav() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Saldo", Path: "/wallet", Active: true},
		{Label: "Histórico", Path: "/wallet/ledger"},
		{Label: "Comprar tokens", Path: "/wallet/topup"},
	}
}

func topupNav() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Saldo", Path: "/wallet"},
		{Label: "Histórico", Path: "/wallet/ledger"},
		{Label: "Comprar tokens", Path: "/wallet/topup", Active: true},
	}
}

func ledgerNav() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Saldo", Path: "/wallet"},
		{Label: "Histórico", Path: "/wallet/ledger", Active: true},
		{Label: "Comprar tokens", Path: "/wallet/topup"},
	}
}

func userMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Configurar 2FA", Path: "/admin/2fa/setup"},
		{Label: "Sair", Path: "/logout", Form: true},
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// entryViewFrom projects an adapter LedgerEntryView onto the template
// view model. All Portuguese labels live here so the template stays
// declarative.
func entryViewFrom(e LedgerEntryView) ledgerEntryView {
	view := ledgerEntryView{
		Kind:              string(e.Kind),
		KindLabel:         kindLabel(e.Kind),
		SourceLabel:       sourceLabel(e.Source),
		OccurredAtISO:     e.OccurredAt.UTC().Format(time.RFC3339),
		OccurredAtLabel:   e.OccurredAt.UTC().Format("02/01/2006 15:04 MST"),
		Direction:         direction(e.Amount),
		AmountLabel:       signedHumanInt(e.Amount),
		BalanceAfterLabel: humanInt(e.BalanceAfter),
		Model:             e.Model,
	}
	if e.ConversationIDHash != "" {
		// Render only the first eight chars so the chip stays compact;
		// LGPD-safe — the full hash is still in the adapter's payload
		// for support staff with master scope.
		short := e.ConversationIDHash
		if len(short) > 8 {
			short = short[:8]
		}
		view.ConversationIDHash = short
	}
	if e.PolicyID != uuid.Nil {
		s := e.PolicyID.String()
		view.PolicyID = s
		view.PolicyIDShort = s[:8]
	}
	return view
}

func kindLabel(k wallet.LedgerKind) string {
	switch k {
	case wallet.KindCommit:
		return "Consumo confirmado"
	case wallet.KindReserve:
		return "Consumo reservado"
	case wallet.KindRelease:
		return "Reserva liberada"
	case wallet.KindGrant:
		return "Crédito"
	default:
		return string(k)
	}
}

func sourceLabel(s wallet.LedgerSource) string {
	switch s {
	case wallet.SourceConsumption:
		return "Consumo LLM"
	case wallet.SourceMonthlyAlloc:
		return "Renovação mensal"
	case wallet.SourceMasterGrant:
		return "Cortesia master"
	default:
		return string(s)
	}
}

func direction(amount int64) string {
	if amount < 0 {
		return "debit"
	}
	return "credit"
}

// humanInt formats an int with thin-space thousand separators using
// the Brazilian convention (1.234.567). Negative numbers carry their
// sign.
func humanInt(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func signedHumanInt(n int64) string {
	if n > 0 {
		return "+" + humanInt(n)
	}
	return humanInt(n)
}

// formatBRL renders cents as "R$ 1.234,56" — same shape as the billing
// invoice list.
func formatBRL(cents int) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	r := cents % 100
	whole := humanInt(int64(cents / 100))
	prefix := ""
	if negative {
		prefix = "-"
	}
	return fmt.Sprintf("R$ %s%s,%02d", prefix, whole, r)
}
