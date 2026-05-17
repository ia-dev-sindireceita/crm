package aipanel

import (
	"html/template"
	"io"
)

// ConsentModalData backs consentModalTmpl. All fields are plain strings
// — there is intentionally NO template.HTML on the preview path so the
// Go template auto-escape stays the only sanitiser between the
// anonymized payload and the rendered DOM (F29 mitigation, SIN-62225).
//
// CSRFInput is a pre-escaped <input type="hidden" name="_csrf"…> that
// the inbox handler builds via csrf.FormHidden before calling
// RenderConsentModal. Keeping it pre-baked here means the consent
// adapter does not need to import the csrf helper transitively.
type ConsentModalData struct {
	// ScopeKind is the aipolicy.ScopeType value rendered as text
	// ("tenant" | "team" | "channel"). The hidden form input embeds it
	// so the accept handler can rebuild the consent scope without
	// re-resolving it server-side.
	ScopeKind string
	// ScopeID is the consent scope id text — tenant id for tenant
	// scope, team id text for team scope, channel id text for channel.
	ScopeID string
	// Payload is the anonymized preview the operator MUST inspect. The
	// gate (internal/aiassist/usecase.checkConsent) populates this from
	// Anonymizer.Anonymize and the caller MUST pass it through verbatim
	// — the template auto-escape is the only encoder.
	Payload string
	// AnonymizerVersion + PromptVersion mirror the active versions the
	// gate ran against. They flow back on the accept body so the
	// service stores them as a re-consent trigger (AC #4 of SIN-62352).
	AnonymizerVersion string
	PromptVersion     string
	// PayloadHashHex is the full sha256 hex of Payload (64 chars). The
	// template renders the first 12 as a visual audit aid AND embeds
	// the full digest as a hidden form input so the accept handler can
	// detect tampering.
	PayloadHashHex string
	// ConversationID is the inbox conversation the original request
	// targeted. The accept handler echoes it back via HX-Trigger so
	// the client can re-fire the original aiassist call against the
	// same conversation. Empty when the caller is not the inbox (e.g.
	// future contexts without a conversation surface).
	ConversationID string
	// CSRFInput is the project-standard <input type="hidden"
	// name="_csrf" value="…"> snippet the inbox handler pre-renders
	// from csrf.FormHidden(token).
	CSRFInput template.HTML
}

// RenderConsentModal writes the consent modal HTML to w. The caller MUST
// set the response status and Content-Type before calling — the inbox
// handler typically writes status 200 with HX-Retarget="#ai-consent-modal"
// + HX-Reswap="innerHTML" so HTMX redirects the swap from the assist
// panel to the modal anchor.
//
// Returning the template error lets the caller log it; the HTTP response
// is already committed at that point so there is no recovery path beyond
// logging.
func RenderConsentModal(w io.Writer, data ConsentModalData) error {
	return consentModalTmpl.Execute(w, data)
}

// shortHashChars is the prefix length of the SHA-256 hex digest the
// modal renders as a visual fingerprint. Twelve hex chars = 48 bits —
// enough for an operator to spot when the preview changes between
// calls; the full digest goes into the hidden form input for the
// anti-tampering compare.
const shortHashChars = 12

// shortHash returns the first shortHashChars of full, or full itself
// when it is shorter. Used by the template via the FuncMap.
func shortHash(full string) string {
	if len(full) <= shortHashChars {
		return full
	}
	return full[:shortHashChars]
}

// consentModalTmpl renders the accessibility-correct modal markup. The
// template intentionally writes the OUTER element with id
// "ai-consent-modal" so HTMX's outerHTML swap from the cancel handler
// can replace the whole modal with an empty placeholder of the same
// id (keeping the DOM anchor for any future re-trigger of the gate).
//
// Accessibility:
//   - role="dialog" + aria-modal="true" + aria-labelledby="…" form the
//     ARIA dialog pattern.
//   - autofocus on the Confirmar button puts the keyboard focus on the
//     primary action when the modal swaps in.
//   - The Cancelar button carries hx-trigger="click, keyup[key=='Escape']
//     from:body" so pressing ESC anywhere on the page dismisses the
//     modal (the from:body filter ensures the trigger fires regardless
//     of the currently-focused element).
//
// Both buttons use type="button" so the modal swap is not accidentally
// submitted as the parent compose form (the inbox view nests the modal
// inside an <aside>, but defensive type="button" survives a future
// reparent).
var consentModalTmpl = template.Must(template.New("consent_modal").Funcs(template.FuncMap{
	"shortHash": shortHash,
}).Parse(`<section id="ai-consent-modal"
         class="ai-consent-modal"
         role="dialog"
         aria-modal="true"
         aria-labelledby="ai-consent-heading">
  <header class="ai-consent-modal__header">
    <h2 id="ai-consent-heading" class="ai-consent-modal__title">Confirme o envio para o OpenRouter</h2>
  </header>
  <p class="ai-consent-modal__lead">
    O conteúdo abaixo, já anonimizado, será enviado ao OpenRouter para gerar a sugestão.
    Após confirmar, novas mensagens neste escopo não pedem confirmação até a versão do
    anonimizador ou do prompt mudar.
  </p>
  <pre class="ai-consent-modal__payload" aria-label="Payload anonimizado">{{.Payload}}</pre>
  <dl class="ai-consent-modal__meta">
    <dt>Anonimizador</dt><dd>{{.AnonymizerVersion}}</dd>
    <dt>Prompt</dt><dd>{{.PromptVersion}}</dd>
    <dt>SHA-256 (prefixo)</dt><dd><code class="ai-consent-modal__hash">{{shortHash .PayloadHashHex}}</code></dd>
  </dl>
  <div class="ai-consent-modal__actions">
    <div class="ai-consent-modal__form ai-consent-modal__form--accept">
      {{.CSRFInput}}
      <input type="hidden" name="scope_kind" value="{{.ScopeKind}}">
      <input type="hidden" name="scope_id" value="{{.ScopeID}}">
      <input type="hidden" name="anonymizer_version" value="{{.AnonymizerVersion}}">
      <input type="hidden" name="prompt_version" value="{{.PromptVersion}}">
      <input type="hidden" name="payload_hash" value="{{.PayloadHashHex}}">
      <input type="hidden" name="payload_preview" value="{{.Payload}}">
      {{if .ConversationID}}<input type="hidden" name="conversation_id" value="{{.ConversationID}}">{{end}}
      <button type="button" autofocus
              class="ai-consent-modal__btn ai-consent-modal__btn--primary"
              hx-post="/aipanel/consent/accept"
              hx-target="#ai-consent-modal"
              hx-swap="outerHTML"
              hx-include="closest .ai-consent-modal__form--accept">Confirmar e enviar</button>
    </div>
    <div class="ai-consent-modal__form ai-consent-modal__form--cancel">
      {{.CSRFInput}}
      <input type="hidden" name="scope_kind" value="{{.ScopeKind}}">
      <button type="button"
              class="ai-consent-modal__btn ai-consent-modal__btn--secondary"
              hx-post="/aipanel/consent/cancel"
              hx-target="#ai-consent-modal"
              hx-swap="outerHTML"
              hx-trigger="click, keyup[key=='Escape'] from:body"
              hx-include="closest .ai-consent-modal__form--cancel">Cancelar</button>
    </div>
  </div>
</section>
`))

// cancelledPlaceholderTmpl is the empty section the cancel handler
// returns. Keeping the #ai-consent-modal anchor means a subsequent gate
// trigger can swap a fresh modal back in without re-rendering the
// surrounding inbox layout.
var cancelledPlaceholderTmpl = template.Must(template.New("consent_modal_cancelled").Parse(
	`<section id="ai-consent-modal" class="ai-consent-modal ai-consent-modal--cancelled" hidden></section>
`))
