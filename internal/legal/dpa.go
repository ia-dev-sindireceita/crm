// Package legal exposes the platform-level Data Processing Agreement
// (DPA) text and the canonical list of sub-processors. Both are
// versioned together: a change to the markdown OR a change to the
// sub-processor list MUST bump DPAVersion, because the platform
// commits to notifying tenants 30 days before any sub-processor change
// (see DPA §4).
//
// The web layer (internal/web/privacy) reads from this package only;
// no DB I/O lives here. Keeping the surface tiny and read-only means
// the privacy page renders in microseconds and stays trivially
// auditable: one map of constants, one embedded markdown blob.
//
// Wired by SIN-62354 to satisfy decisão #8 / SIN-62203 (LGPD
// sub-processor disclosure requirement, Fase 3).
package legal

import (
	_ "embed"
	"strings"
)

// DPAVersion is the canonical version string for the platform DPA. Bump
// this whenever dpa.md changes OR the Subprocessors() list changes —
// the tenant-notification clock (30 days) starts on the change of this
// constant, so cosmetic edits should bundle with substantive ones.
//
// Semver: MAJOR.MINOR.PATCH where:
//   - MAJOR bumps when contractual obligations change (retention,
//     erasure SLAs, security baseline).
//   - MINOR bumps when a sub-processor is added/removed/replaced.
//   - PATCH bumps for clarifying-only edits (typos, formatting) that
//     do NOT trigger the 30-day notice.
const DPAVersion = "1.0.0"

//go:embed dpa.md
var dpaMarkdown string

// DPAMarkdown returns the embedded DPA markdown. The string is
// immutable for the life of the binary — a new release ships a new
// version of the document and a new DPAVersion.
func DPAMarkdown() string {
	return dpaMarkdown
}

// DPAContentType is the MIME type returned by the download endpoint.
// text/markdown is the IANA-registered type (RFC 7763); browsers fall
// back to text/plain rendering for unknown types, which is what we
// want for the "save as" UX.
const DPAContentType = "text/markdown; charset=utf-8"

// DPAFilename is the filename the download endpoint uses in
// Content-Disposition. The version suffix lets a tenant keep multiple
// versions on disk without manual renaming. The endpoint is free to
// append a timestamp at request time if it wants a unique name per
// download.
func DPAFilename() string {
	return "dpa-sindireceita-v" + DPAVersion + ".md"
}

// SubprocessorKind classifies a sub-processor by the broad capability
// it provides. The privacy page groups by kind so the LGPD reader can
// scan quickly: "what AI vendor do they use", "what messaging
// vendor", etc. Free-form strings keep the type extensible without
// schema migration.
type SubprocessorKind string

const (
	KindAI        SubprocessorKind = "ai"
	KindMessaging SubprocessorKind = "messaging"
	KindEmail     SubprocessorKind = "email"
	KindPayment   SubprocessorKind = "payment"
)

// Subprocessor is the row shape rendered on /settings/privacy and
// referenced in DPA §4. The fields mirror what an LGPD reviewer
// expects to see at a glance — name, what they do, what data they
// touch, where to read their own privacy policy.
//
// All fields are required EXCEPT PolicyURL on a Status="pending"
// row (e.g. the PIX PSP placeholder), where the URL is not yet
// known.
type Subprocessor struct {
	// Name is the legal/brand name the tenant will recognise
	// (e.g. "OpenRouter, Inc.", "Meta Platforms").
	Name string

	// Kind classifies the vendor.
	Kind SubprocessorKind

	// Purpose is the one-sentence finality of the processing,
	// matching DPA §4.x "Finalidade" rows.
	Purpose string

	// DataHandled describes the categories of personal data sent
	// to the sub-processor, matching DPA §4.x "Dados tratados"
	// rows. PII-mask state should be stated here when relevant
	// (e.g. "mensagens com PII estruturada mascarada" for
	// OpenRouter per ADR-0041 / decisão #8).
	DataHandled string

	// PolicyURL is an external link to the sub-processor's own
	// privacy / DPA page. Empty when Status="pending".
	PolicyURL string

	// Status is "active" for live sub-processors and "pending"
	// for placeholders that depend on a separate decision
	// (e.g. PIX PSP awaiting ratification).
	Status string
}

// Subprocessors returns the canonical list of active and pending
// sub-processors, in the order they should render on
// /settings/privacy. The order is intentional: AI first (the
// LGPD-sensitive one per decisão #8), then transport (Meta), then
// transactional email, then payments.
//
// A copy is returned so callers cannot mutate the package-level
// truth.
func Subprocessors() []Subprocessor {
	src := canonicalSubprocessors
	out := make([]Subprocessor, len(src))
	copy(out, src)
	return out
}

// canonicalSubprocessors is the single source of truth referenced by
// Subprocessors(). Mirrors DPA §4. ANY edit here MUST bump DPAVersion
// and the regression test in subprocessors_test.go expects this
// invariant (it asserts the test sees exactly these names so that an
// accidental silent edit fails CI).
var canonicalSubprocessors = []Subprocessor{
	{
		Name:        "OpenRouter, Inc.",
		Kind:        KindAI,
		Purpose:     "Resumir conversa e sugerir argumentação de venda.",
		DataHandled: "Mensagens com PII estruturada mascarada (telefone, e-mail e CPF substituídos por tokens; nomes mantidos por necessidade conversacional).",
		PolicyURL:   "https://openrouter.ai/privacy",
		Status:      "active",
	},
	{
		Name:        "Meta Platforms (WhatsApp / Instagram / Facebook)",
		Kind:        KindMessaging,
		Purpose:     "Transporte de mensagens via WhatsApp Cloud API, Instagram Graph API e Messenger Graph API.",
		DataHandled: "Mensagens, mídias e metadados de identificação do cliente final no canal correspondente.",
		PolicyURL:   "https://www.facebook.com/legal/terms/dataprocessingterms",
		Status:      "active",
	},
	{
		Name:        "Mailgun Technologies",
		Kind:        KindEmail,
		Purpose:     "Entrega transacional de e-mail (notificações de cobrança, recuperação de senha, convites).",
		DataHandled: "Endereço de e-mail e conteúdo da mensagem transacional.",
		PolicyURL:   "https://www.mailgun.com/dpa/",
		Status:      "active",
	},
	{
		Name:        "PSP de PIX (a definir — decisão D2)",
		Kind:        KindPayment,
		Purpose:     "Conciliação de cobranças PIX.",
		DataHandled: "Identificadores de transação e dados bancários do pagador (escopo final dependente do provedor ratificado em D2).",
		PolicyURL:   "",
		Status:      "pending",
	},
}

// DPAMentionsOpenRouter is a cheap invariant checked by tests and
// (defensively) by the handler: the DPA markdown MUST mention
// "OpenRouter" because that is the entire point of decisão #8.
// Returning false would mean someone shipped a broken DPA and the
// privacy page must refuse to render.
func DPAMentionsOpenRouter() bool {
	return strings.Contains(dpaMarkdown, "OpenRouter")
}
