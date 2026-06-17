package walletui

import (
	"embed"
	"html/template"

	"github.com/pericles-luz/crm/internal/web/shell"
)

//go:embed dashboard.html topup.html ledger.html ledger_rows.html
var templateAssets embed.FS

// dashboardTmpl renders GET /wallet — saldo + projeção + ledger preview.
// Uses shell.Layout so the chrome (top-bar, branded theme, user-menu)
// stays consistent with the rest of the authenticated surface.
var dashboardTmpl = shell.MustParse(nil, templateAssets, "dashboard.html")

// topupTmpl renders GET /wallet/topup — catálogo de pacotes.
var topupTmpl = shell.MustParse(nil, templateAssets, "topup.html")

// ledgerLayoutTmpl renders GET /wallet/ledger — the full ledger page,
// including the embedded rows partial used by the HTMX "Carregar mais"
// swap.
var ledgerLayoutTmpl = shell.MustParse(nil, templateAssets, "ledger.html", "ledger_rows.html")

// ledgerRowsTmpl renders the partial that the HTMX swap target replaces
// when the user clicks "Carregar mais" on the ledger page.
var ledgerRowsTmpl = template.Must(template.New("wallet_ledger_rows").ParseFS(templateAssets, "ledger_rows.html"))
