// SIN-62908 — Fase 3 W4D AI-assist handler.
//
// The button "Resumir + sugerir 3 respostas" lives in the conversation
// view pane (see conversationViewTmpl in templates.go). Clicking it
// POSTs to /inbox/conversations/:id/ai-assist; the handler asks the
// aiassist use case to produce a structured "summary + 3 suggestions"
// completion, parses the result, and renders an HTMX partial that is
// swapped beforeend into the assist panel.
//
// The render is intentionally NOT a streaming SSE response: the
// requirement is the operator-visible appearance of "summary first,
// suggestions below", which we obtain with a single re-render that
// stacks two sub-partials in the same swap. Streaming-over-fetch keeps
// the implementation HTMX-native and avoids the long-lived connection
// budget SSE would impose on the rate-limit gate (SIN-62238).
//
// Three error states are distinguished:
//
//   - aiassist.ErrInsufficientBalance → balance banner, no retry.
//   - aiassist.ErrAIDisabled         → button disabled with tooltip.
//   - aiassist.ErrRateLimited        → toast "Aguarde 30s antes de re-tentar".
//
// Anything else (LLM unavailable / decode failures / unexpected) maps
// to a generic 503 panel so the operator can retry later.
//
// ProductArgument context is folded into the prompt via the
// ProductArgumentLister port. The handler walks the (tenant, channel,
// team) scope and prepends the most-specific argument text to the LLM
// prompt so the suggestions reflect the seller's pitch — sanity test
// covers AC #7 of SIN-62196.

package inbox

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// AssistRoutePath is the HTMX endpoint the button POSTs to. Exported so
// the conversation-view template can render the same path the router
// mounts (no template-time string drift).
const AssistRoutePath = "/inbox/conversations/{id}/ai-assist"

// AssistSummarizer is the use-case port the handler calls to generate
// (summary + suggestions). It is satisfied structurally by
// *aiassistusecase.Service; the interface stays here so tests can drop
// in a small fake without dragging the wallet / LLM port surface.
type AssistSummarizer interface {
	Summarize(ctx context.Context, req aiassistusecase.SummarizeRequest) (*aiassistusecase.SummarizeResponse, error)
}

// AssistPolicyChecker is the read-only port the handler calls to decide
// whether the "resumir" button should render in the enabled state. A
// nil checker keeps the legacy behaviour (button always enabled,
// policy gating still applied server-side by Summarize). The check is
// best-effort cosmetic: even if it returns "enabled" while the policy
// is in fact disabled, the click POST surfaces the proper
// policy_disabled partial via Summarize's ErrAIDisabled return.
type AssistPolicyChecker interface {
	IsEnabled(ctx context.Context, tenantID uuid.UUID, channelID, teamID string) (bool, error)
}

// AssistProductArgumentLister is the catalog port the handler calls to
// fold ProductArgument text into the LLM prompt. ProductID is the
// product the conversation is about; pass uuid.Nil to skip arguments
// entirely. The returned slice is ordered most-specific first (channel
// > team > tenant); the handler folds the first non-empty entry into
// the prompt so a single channel pitch wins over the catch-all.
type AssistProductArgumentLister interface {
	List(ctx context.Context, tenantID, productID uuid.UUID, channelID, teamID string) ([]string, error)
}

// AssistMetrics is the observability surface the handler emits on
// every call. nil is safe (the handler skips emission).
type AssistMetrics struct {
	// Duration is the seconds-resolution histogram of end-to-end
	// Summarize latency, labelled by outcome (ok | cache_hit |
	// error). Buckets are tuned around the AC #1 8s p95 budget so
	// regressions surface as a shift across the 1s/3s/8s edges.
	Duration *prometheus.HistogramVec
	// Errors counts error outcomes by reason. Cardinality is bounded
	// to the closed reason set (insufficient_balance | policy_disabled
	// | rate_limited | llm_unavailable | internal).
	Errors *prometheus.CounterVec
}

// NewAssistMetrics constructs the metrics surface against reg. reg may
// be nil — the counters are returned unregistered (the pattern tests
// use to keep registries isolated).
func NewAssistMetrics(reg prometheus.Registerer) *AssistMetrics {
	m := &AssistMetrics{
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ai_assist_summarize_duration_seconds",
			Help:    "End-to-end ai-assist Summarize latency in seconds, labelled by outcome (ok | cache_hit | error). [SIN-62908]",
			Buckets: []float64{0.25, 0.5, 1, 2, 3, 5, 8, 13, 21},
		}, []string{"outcome"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_assist_summarize_errors_total",
			Help: "Total ai-assist Summarize errors, partitioned by reason. [SIN-62908]",
		}, []string{"reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.Duration, m.Errors)
	}
	return m
}

// AssistDeps bundles the assist-feature collaborators. All fields are
// optional from the wider Handler's perspective — when Summarizer is
// nil the button + POST route are not registered, so existing
// inbox-only deployments keep the same surface.
type AssistDeps struct {
	// Summarizer is the use-case port. Required to activate the
	// feature; when nil the assist routes / button are off entirely.
	Summarizer AssistSummarizer
	// Policy is the optional read-side gate for the button-enabled
	// cosmetic check. nil → assume enabled, defer to Summarizer.
	Policy AssistPolicyChecker
	// Arguments is the optional catalog port for prompt-folding. nil →
	// no product context (the LLM still produces a generic reply).
	Arguments AssistProductArgumentLister
	// Metrics is the optional Prometheus surface. nil disables.
	Metrics *AssistMetrics
	// RequestID returns a per-call idempotency token. The handler
	// embeds it as the third leg of the (tenant, conversation,
	// request_id) idempotency triple Summarize uses to dedup wallet
	// reservations on retry. nil falls back to a clock-derived token.
	RequestID func(*http.Request) string
	// ProductID resolves the product the conversation is about, if
	// known. Returns uuid.Nil for "no product context" — the handler
	// then skips ProductArgument folding. nil port behaves like
	// "always uuid.Nil".
	ProductID func(*http.Request) uuid.UUID
	// MaxPromptChars caps the prompt size sent to the LLM. The
	// per-message bubble at maxBodyChars (4096) means a 50-message
	// conversation fits well under 200KB; capping at 200_000 protects
	// against pathological payloads without trimming honest traffic.
	// Zero falls back to 200_000.
	MaxPromptChars int
}

// assistMaxPromptCharsDefault is the default cap when AssistDeps.MaxPromptChars
// is zero. The 200_000 ceiling is approximately the longest payload a
// conversation that has stayed under the per-message length limit can
// produce after 50 turns of mixed inbound + outbound text.
const assistMaxPromptCharsDefault = 200_000

// assistOutcome / assistReason are the closed enums emitted to
// Prometheus labels so the registry stays bounded.
const (
	assistOutcomeOK       = "ok"
	assistOutcomeCacheHit = "cache_hit"
	assistOutcomeError    = "error"

	assistReasonInsufficientBalance = "insufficient_balance"
	assistReasonPolicyDisabled      = "policy_disabled"
	assistReasonRateLimited         = "rate_limited"
	assistReasonLLMUnavailable      = "llm_unavailable"
	assistReasonInternal            = "internal"
)

// aiAssist is the POST handler for the assist button. It composes the
// prompt from the persisted conversation messages, folds ProductArgument
// text in, calls Summarize, and renders the appropriate partial.
func (h *Handler) aiAssist(w http.ResponseWriter, r *http.Request) {
	if h.deps.AIAssist.Summarizer == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	conversationID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid conversation id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	channelID := strings.TrimSpace(r.PostFormValue("channelId"))
	teamID := strings.TrimSpace(r.PostFormValue("teamId"))

	messages, err := h.deps.ListMessages.Execute(r.Context(), inboxusecase.ListMessagesInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	})
	if err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "list messages for assist", err)
		return
	}

	productID := uuid.Nil
	if h.deps.AIAssist.ProductID != nil {
		productID = h.deps.AIAssist.ProductID(r)
	}
	arguments, err := h.listAssistArguments(r.Context(), tenant.ID, productID, channelID, teamID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list product arguments", err)
		return
	}

	views := make([]assistMessageView, 0, len(messages.Items))
	for _, m := range messages.Items {
		views = append(views, assistMessageView{
			Direction: m.Direction,
			Body:      m.Body,
		})
	}
	prompt := buildAssistPrompt(arguments, views, h.assistPromptCap())
	requestID := h.assistRequestID(r)

	start := time.Now()
	resp, summarizeErr := h.deps.AIAssist.Summarizer.Summarize(r.Context(), aiassistusecase.SummarizeRequest{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
		RequestID:      requestID,
		Prompt:         prompt,
		Scope: aiassist.Scope{
			TeamID:    teamID,
			ChannelID: channelID,
		},
	})
	elapsed := time.Since(start).Seconds()

	if summarizeErr != nil {
		h.renderAssistError(w, summarizeErr, elapsed)
		return
	}

	outcome := assistOutcomeOK
	if resp != nil && resp.CacheHit {
		outcome = assistOutcomeCacheHit
	}
	h.observeAssistDuration(outcome, elapsed)

	view := newAssistPanel(resp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := assistPanelTmpl.Execute(w, view); err != nil {
		h.deps.Logger.Error("web/inbox: render assist panel", "err", err)
	}
}

// renderAssistError centralises the error → partial mapping so each
// branch gets the right HTTP status + Prometheus reason label.
func (h *Handler) renderAssistError(w http.ResponseWriter, err error, elapsed float64) {
	h.observeAssistDuration(assistOutcomeError, elapsed)

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	switch {
	case errors.Is(err, aiassist.ErrInsufficientBalance):
		h.incAssistError(assistReasonInsufficientBalance)
		w.WriteHeader(http.StatusPaymentRequired)
		_ = assistBalanceBannerTmpl.Execute(w, nil)
	case errors.Is(err, aiassist.ErrAIDisabled):
		h.incAssistError(assistReasonPolicyDisabled)
		w.WriteHeader(http.StatusForbidden)
		_ = assistPolicyDisabledTmpl.Execute(w, nil)
	case errors.Is(err, aiassist.ErrRateLimited):
		h.incAssistError(assistReasonRateLimited)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = assistRateLimitedTmpl.Execute(w, nil)
	case errors.Is(err, aiassist.ErrLLMUnavailable):
		h.incAssistError(assistReasonLLMUnavailable)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = assistLLMUnavailableTmpl.Execute(w, nil)
	default:
		h.deps.Logger.Error("web/inbox: ai-assist failed", "err", err)
		h.incAssistError(assistReasonInternal)
		w.WriteHeader(http.StatusInternalServerError)
		_ = assistLLMUnavailableTmpl.Execute(w, nil)
	}
}

// listAssistArguments is a nil-tolerant call to the catalog port. The
// handler treats a missing port or a uuid.Nil product as "no
// arguments"; an actual lister error bubbles up so the 500 path runs.
func (h *Handler) listAssistArguments(
	ctx context.Context,
	tenantID, productID uuid.UUID,
	channelID, teamID string,
) ([]string, error) {
	if h.deps.AIAssist.Arguments == nil || productID == uuid.Nil {
		return nil, nil
	}
	return h.deps.AIAssist.Arguments.List(ctx, tenantID, productID, channelID, teamID)
}

// assistRequestID picks the boundary idempotency key. The
// AssistDeps.RequestID hook lets the composition root inject a
// session-derived token (e.g. the CSRF random); the clock-derived
// fallback works for tests and keeps the contract non-nil.
func (h *Handler) assistRequestID(r *http.Request) string {
	if h.deps.AIAssist.RequestID != nil {
		if id := strings.TrimSpace(h.deps.AIAssist.RequestID(r)); id != "" {
			return id
		}
	}
	return fmt.Sprintf("assist-%d", time.Now().UnixNano())
}

// assistPromptCap returns the configured prompt-size ceiling, applying
// the default when MaxPromptChars is zero.
func (h *Handler) assistPromptCap() int {
	if h.deps.AIAssist.MaxPromptChars > 0 {
		return h.deps.AIAssist.MaxPromptChars
	}
	return assistMaxPromptCharsDefault
}

// observeAssistDuration emits the histogram sample when metrics are
// wired. nil metrics is a no-op so unit tests don't have to register
// with a global registry.
func (h *Handler) observeAssistDuration(outcome string, seconds float64) {
	if h.deps.AIAssist.Metrics == nil || h.deps.AIAssist.Metrics.Duration == nil {
		return
	}
	h.deps.AIAssist.Metrics.Duration.WithLabelValues(outcome).Observe(seconds)
}

// incAssistError emits the error counter when metrics are wired.
func (h *Handler) incAssistError(reason string) {
	if h.deps.AIAssist.Metrics == nil || h.deps.AIAssist.Metrics.Errors == nil {
		return
	}
	h.deps.AIAssist.Metrics.Errors.WithLabelValues(reason).Inc()
}

// buildAssistPrompt folds the (most-specific) product argument into a
// fixed-format prompt the LLM is asked to follow. The format is
// deliberately strict so the parser in newAssistPanel can split
// reliably; if the LLM disrespects it, the panel falls back to a
// single Summary block plus zero suggestions.
//
// The prompt is capped at maxChars; older messages are trimmed first
// (we keep the most recent context so the reply stays on-topic).
func buildAssistPrompt(arguments []string, messages []assistMessageView, maxChars int) string {
	var b strings.Builder
	b.WriteString("Você é um atendente de suporte ao cliente que responde em pt-BR.\n")
	b.WriteString("Sua tarefa:\n")
	b.WriteString("1. Resumir a conversa abaixo em 2-3 frases curtas.\n")
	b.WriteString("2. Sugerir EXATAMENTE 3 respostas concisas que o atendente pode enviar.\n\n")
	b.WriteString("Responda usando este formato EXATO, sem nada antes ou depois:\n")
	b.WriteString("RESUMO: <texto>\n")
	b.WriteString("SUGESTAO 1: <texto>\n")
	b.WriteString("SUGESTAO 2: <texto>\n")
	b.WriteString("SUGESTAO 3: <texto>\n\n")
	for _, arg := range arguments {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		b.WriteString("Contexto comercial relevante: ")
		b.WriteString(arg)
		b.WriteString("\n\n")
		break
	}
	b.WriteString("Conversa:\n")
	// Render oldest-first so the LLM sees the chronological order.
	for _, m := range messages {
		role := "Cliente"
		if m.Direction == "out" {
			role = "Atendente"
		}
		body := strings.TrimSpace(m.Body)
		if body == "" {
			continue
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(body)
		b.WriteString("\n")
	}
	out := b.String()
	if maxChars > 0 && len(out) > maxChars {
		// Trim from the front (oldest messages) while keeping the
		// instruction header intact. We find the "Conversa:" anchor
		// and chop the lines after it until we fit.
		out = trimAssistPrompt(out, maxChars)
	}
	return out
}

// trimAssistPrompt drops oldest-first message lines until the prompt
// fits maxChars. The instruction header is preserved verbatim because
// the parser depends on the strict response format the prompt declares.
func trimAssistPrompt(prompt string, maxChars int) string {
	anchor := "Conversa:\n"
	idx := strings.Index(prompt, anchor)
	if idx < 0 {
		// No conversation block — instruction-only prompt; truncate at
		// the end to fit.
		if len(prompt) > maxChars {
			return prompt[:maxChars]
		}
		return prompt
	}
	header := prompt[:idx+len(anchor)]
	body := prompt[idx+len(anchor):]
	lines := strings.Split(body, "\n")
	for len(lines) > 1 && len(header)+totalLen(lines) > maxChars {
		// Drop the oldest line (index 0) until we fit.
		lines = lines[1:]
	}
	return header + strings.Join(lines, "\n")
}

// totalLen sums the byte length of lines including the per-line
// newline char that strings.Join restores. The +len(lines)-1 term
// accounts for the joining newlines.
func totalLen(lines []string) int {
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if len(lines) > 0 {
		total += len(lines) - 1
	}
	return total
}

// assistPanelView is what the panel template renders. Summary may be
// empty when the LLM returned no parseable summary block; Suggestions
// may be empty when parsing yielded zero suggestions — the template
// degrades cleanly in both cases.
type assistPanelView struct {
	Summary     string
	Suggestions []string
	CacheHit    bool
	Model       string
}

// newAssistPanel parses the (Summary.Text) string the use case
// returned. The LLM is prompted to emit a fixed format; deviations
// fall back to "show the raw text as the summary, no suggestions".
func newAssistPanel(resp *aiassistusecase.SummarizeResponse) assistPanelView {
	view := assistPanelView{}
	if resp == nil || resp.Summary == nil {
		return view
	}
	view.CacheHit = resp.CacheHit
	view.Model = resp.Summary.Model
	parsed := parseAssistText(resp.Summary.Text)
	view.Summary = parsed.summary
	view.Suggestions = parsed.suggestions
	return view
}

// parseAssistText scans the LLM output for the RESUMO / SUGESTAO 1..3
// markers in the documented order. The matcher is intentionally
// permissive about whitespace and casing so a slightly off-format
// reply still extracts useful content. When the input has no markers
// at all, the whole string becomes the summary and no suggestions are
// returned — the panel template renders the summary block only.
func parseAssistText(text string) struct {
	summary     string
	suggestions []string
} {
	out := struct {
		summary     string
		suggestions []string
	}{}
	if strings.TrimSpace(text) == "" {
		return out
	}
	lines := strings.Split(text, "\n")
	var (
		section string
		summary []string
		s1      []string
		s2      []string
		s3      []string
	)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "RESUMO:"):
			section = "summary"
			summary = append(summary, strings.TrimSpace(line[len("RESUMO:"):]))
		case strings.HasPrefix(upper, "SUGESTAO 1:") || strings.HasPrefix(upper, "SUGESTÃO 1:"):
			section = "s1"
			s1 = append(s1, strings.TrimSpace(stripMarkerPrefix(line, "SUGESTAO 1:", "SUGESTÃO 1:")))
		case strings.HasPrefix(upper, "SUGESTAO 2:") || strings.HasPrefix(upper, "SUGESTÃO 2:"):
			section = "s2"
			s2 = append(s2, strings.TrimSpace(stripMarkerPrefix(line, "SUGESTAO 2:", "SUGESTÃO 2:")))
		case strings.HasPrefix(upper, "SUGESTAO 3:") || strings.HasPrefix(upper, "SUGESTÃO 3:"):
			section = "s3"
			s3 = append(s3, strings.TrimSpace(stripMarkerPrefix(line, "SUGESTAO 3:", "SUGESTÃO 3:")))
		default:
			// Continuation: append to the most recent section.
			switch section {
			case "summary":
				summary = append(summary, line)
			case "s1":
				s1 = append(s1, line)
			case "s2":
				s2 = append(s2, line)
			case "s3":
				s3 = append(s3, line)
			}
		}
	}
	if len(summary) == 0 && section == "" {
		// No markers at all — keep the raw text as the summary so the
		// operator at least gets something useful.
		out.summary = strings.TrimSpace(text)
		return out
	}
	out.summary = strings.TrimSpace(strings.Join(summary, " "))
	for _, sg := range [][]string{s1, s2, s3} {
		joined := strings.TrimSpace(strings.Join(sg, " "))
		if joined != "" {
			out.suggestions = append(out.suggestions, joined)
		}
	}
	return out
}

// stripMarkerPrefix removes whichever case variant of the marker the
// line actually starts with, preserving the rest of the text verbatim.
func stripMarkerPrefix(line string, markers ...string) string {
	upper := strings.ToUpper(line)
	for _, m := range markers {
		if strings.HasPrefix(upper, m) {
			return line[len(m):]
		}
	}
	return line
}

// renderAssistButton emits the "resumir + sugerir 3 respostas" button
// fragment, gated by the policy check when one is wired. The function
// is exported so the conversation_view template can call it through
// the templateFuncs map.
func (h *Handler) renderAssistButton(
	ctx context.Context,
	w io.Writer,
	tenantID uuid.UUID,
	conversationID uuid.UUID,
	channelID, teamID, csrfToken string,
) error {
	if h.deps.AIAssist.Summarizer == nil {
		return nil
	}
	enabled := true
	if h.deps.AIAssist.Policy != nil {
		ok, err := h.deps.AIAssist.Policy.IsEnabled(ctx, tenantID, channelID, teamID)
		if err != nil {
			h.deps.Logger.Warn("web/inbox: policy.IsEnabled failed", "err", err)
			// Fail-open visually; the click POST still hits the gate.
		}
		enabled = ok || err != nil
	}
	data := assistButtonData{
		ConversationID: conversationID,
		ChannelID:      channelID,
		TeamID:         teamID,
		CSRFInput:      csrf.FormHidden(csrfToken),
		Enabled:        enabled,
	}
	return assistButtonTmpl.Execute(w, data)
}

// assistButtonData backs assistButtonTmpl.
type assistButtonData struct {
	ConversationID uuid.UUID
	ChannelID      string
	TeamID         string
	CSRFInput      template.HTML
	Enabled        bool
}

// assistButtonTmpl is the swappable button fragment. Both the enabled
// and disabled variants share the same id ("ai-assist-button"); the
// policy_disabled error partial reuses the disabled markup so the
// click + 403 response transitions the button into the tooltip state
// without a layout shift.
var assistButtonTmpl = template.Must(template.New("ai_assist_button").Parse(`<form class="ai-assist__form"
      hx-post="/inbox/conversations/{{.ConversationID}}/ai-assist"
      hx-target="#ai-assist-panel"
      hx-swap="innerHTML"
      hx-include="this">
  {{.CSRFInput}}
  <input type="hidden" name="channelId" value="{{.ChannelID}}">
  <input type="hidden" name="teamId" value="{{.TeamID}}">
  {{if .Enabled}}
  <button id="ai-assist-button" class="ai-assist__button" type="submit">Resumir + sugerir 3 respostas</button>
  {{else}}
  <button id="ai-assist-button" class="ai-assist__button ai-assist__button--disabled" type="button"
          disabled aria-disabled="true" title="IA desabilitada neste canal">Resumir + sugerir 3 respostas</button>
  {{end}}
</form>
`))

// assistPanelTmpl renders the success state. The summary block lands
// in the panel first, followed by the 3-suggestion list — the operator
// reads the panel top-to-bottom so the "streaming-style" appearance
// is achieved with a single beforeend swap. Each suggestion has a
// button that, when clicked, populates the compose textarea via HTMX
// hx-on::click — the script-free path keeps the page CSP-strict.
var assistPanelTmpl = template.Must(template.New("ai_assist_panel").Parse(`<section class="ai-assist__result" aria-live="polite">
  <header class="ai-assist__result-header">
    <h2 class="ai-assist__result-title">Resumo da conversa</h2>
    {{if .CacheHit}}<span class="ai-assist__cache-hint" title="Resumo servido do cache">cache</span>{{end}}
  </header>
  <p class="ai-assist__summary">{{.Summary}}</p>
  {{if .Suggestions}}
  <h3 class="ai-assist__suggestions-title">Sugestões</h3>
  <ol class="ai-assist__suggestions" role="list">
    {{range $i, $s := .Suggestions}}
    <li class="ai-assist__suggestion">
      <button type="button" class="ai-assist__suggestion-btn"
              data-suggestion="{{$s}}"
              hx-on::click="document.getElementById('compose-body').value = this.dataset.suggestion; document.getElementById('compose-body').focus()">
        {{$s}}
      </button>
    </li>
    {{end}}
  </ol>
  {{end}}
</section>
`))

// assistBalanceBannerTmpl is the no-retry banner for insufficient
// token balance. The copy follows the task spec verbatim; there is no
// retry button because the deficit is a tenant-level decision the
// operator cannot resolve from this surface.
var assistBalanceBannerTmpl = template.Must(template.New("ai_assist_balance").Parse(
	`<div class="ai-assist__banner ai-assist__banner--balance" role="alert">
  <strong>Saldo de tokens esgotado.</strong> Contate o administrador.
</div>
`))

// assistPolicyDisabledTmpl swaps the button into its disabled +
// tooltip state. The id is preserved so a subsequent re-enable (e.g.
// after the admin flips ai_enabled on) can swap back via the same
// target.
var assistPolicyDisabledTmpl = template.Must(template.New("ai_assist_policy").Parse(
	`<div class="ai-assist__banner ai-assist__banner--policy" role="status">
  IA desabilitada neste canal.
</div>
<button id="ai-assist-button" class="ai-assist__button ai-assist__button--disabled" type="button"
        disabled aria-disabled="true" title="IA desabilitada neste canal"
        hx-swap-oob="outerHTML">Resumir + sugerir 3 respostas</button>
`))

// assistRateLimitedTmpl is the rate-limit toast. 30s is the wait
// SIN-62238 documents as the bucket refill window; the literal lives
// in the template (rather than the handler) so a UI tweak doesn't
// require a code change.
var assistRateLimitedTmpl = template.Must(template.New("ai_assist_rate").Parse(
	`<div class="ai-assist__toast ai-assist__toast--rate" role="status">
  Aguarde 30s antes de re-tentar.
</div>
`))

// assistLLMUnavailableTmpl is the generic "transient failure" panel
// that covers ErrLLMUnavailable (non-rate-limit) plus the fallback
// 500 path. The operator can simply click again.
var assistLLMUnavailableTmpl = template.Must(template.New("ai_assist_unavailable").Parse(
	`<div class="ai-assist__banner ai-assist__banner--unavailable" role="alert">
  IA temporariamente indisponível. Tente novamente em alguns instantes.
</div>
`))

// assistMessageView is the minimum slice of MessageView the prompt
// builder needs. Defining the struct locally lets buildAssistPrompt
// be unit-tested without importing the use-case package.
type assistMessageView struct {
	Direction string
	Body      string
}
