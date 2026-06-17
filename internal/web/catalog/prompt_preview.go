package catalog

import (
	"strings"

	"github.com/pericles-luz/crm/internal/catalog"
)

// MaxPreviewArgumentLen caps the argument-text payload accepted by the
// prompt-preview endpoint. The form-level MaxArgumentTextLen is the
// authoritative limit on the persisted argument; the preview accepts up
// to the same length so the operator's debounced live preview never
// truncates mid-edit.
const MaxPreviewArgumentLen = MaxArgumentTextLen

// PromptSegment is one labelled section of the preview pane (system,
// user, argument). The label is operator-facing; the role is the
// stable token the template uses to pick a CSS class for the
// syntax-highlighted box.
type PromptSegment struct {
	Role  string
	Label string
	Text  string
}

// PromptPreview is the rendered, segmented prompt the catalog editor
// shows next to the argument textarea. The three segments mirror the
// system / user / argument split called out in the SIN-63946 spec.
type PromptPreview struct {
	ProductName string
	ScopeLabel  string
	ScopeID     string
	Segments    []PromptSegment
}

// BuildPromptPreview assembles the preview the editor pane renders for
// the supplied (productName, scope, argumentText) tuple. The system
// prompt is a deterministic Portuguese instruction that explains the
// IA's role to the operator; the user segment scaffolds the
// conversation-context block the inbox pipeline injects at call-time;
// the argument segment is the operator's text verbatim so they see
// exactly what will land in the LLM call.
//
// scopeType arrives as the raw string (the form's <select> value) so
// the function gracefully degrades when the operator has not picked a
// scope yet — empty scope renders "—" rather than failing.
func BuildPromptPreview(productName, scopeType, scopeID, argumentText string) PromptPreview {
	productName = strings.TrimSpace(productName)
	if productName == "" {
		productName = "(produto sem nome)"
	}
	scopeLabel := "—"
	if catalog.ScopeType(scopeType).Valid() {
		scopeLabel = scopeLabelText(scopeType)
	}
	if len(argumentText) > MaxPreviewArgumentLen {
		argumentText = argumentText[:MaxPreviewArgumentLen]
	}
	system := "Você é o assistente de vendas da empresa. " +
		"Use o argumento de venda abaixo (verificado pelo gerente) ao " +
		"responder ao cliente sobre \"" + productName + "\". " +
		"Mantenha tom \"neutro\" e idioma pt-BR salvo configuração " +
		"diferente em ai_policy."
	user := "[contexto da conversa do cliente, anonimizado, é inserido " +
		"aqui pelo inbox pipeline em runtime]"
	argTrim := strings.TrimSpace(argumentText)
	if argTrim == "" {
		argTrim = "(argumento vazio — digite acima para pré-visualizar)"
	}
	return PromptPreview{
		ProductName: productName,
		ScopeLabel:  scopeLabel,
		ScopeID:     scopeID,
		Segments: []PromptSegment{
			{Role: "system", Label: "system", Text: system},
			{Role: "user", Label: "user", Text: user},
			{Role: "argument", Label: "argument", Text: argTrim},
		},
	}
}

// scopeLabelText mirrors the templates' scopeLabel func but accepts a
// raw string so non-template callers (the preview builder) can reuse
// the same vocabulary without depending on the template FuncMap.
func scopeLabelText(s string) string {
	switch s {
	case string(catalog.ScopeChannel):
		return "Canal"
	case string(catalog.ScopeTeam):
		return "Equipe"
	case string(catalog.ScopeTenant):
		return "Tenant"
	default:
		return s
	}
}
