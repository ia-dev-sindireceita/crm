package aipolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// LGPDFieldTier classifies a structured contact field by the LGPD
// posture the policy gate must apply. The three tiers mirror the
// SecurityEngineer spec in /SIN/issues/SIN-63945#document-lgpd-field-spec
// (SE doc, rev 1). The literal string values are wire-stable: persisted
// in audit events (field_opt_in.<name>) and embedded in HTML data-tier
// attributes, so renaming is a breaking change.
type LGPDFieldTier string

const (
	// TierGreen names a non-sensitive field that the policy ships
	// cleartext (operational necessity per ADR-0041 D2). Green fields
	// are always included in the prompt context and are NOT stored in
	// Policy.StructuredFields — they are unconditional.
	TierGreen LGPDFieldTier = "green"

	// TierYellow names a PII field tokenised by the anonymizer
	// ([PII:EMAIL], [PII:PHONE], [PII:CNPJ]). Yellow fields require
	// explicit per-policy opt-in; cleartext never leaves regardless of
	// the toggle.
	TierYellow LGPDFieldTier = "yellow"

	// TierRed names an LGPD Art. 5 II / Art. 11 sensitive field. Red
	// fields are NEVER sent to the LLM as structured context. The UI
	// renders the row with disabled + aria-disabled and the policy
	// gate rejects any opt-in via ErrLGPDBlockedField.
	TierRed LGPDFieldTier = "red"
)

// LGPDField is one entry in the closed allow/deny list LGPDFieldCatalog
// returns. Wire-stable values: Name is the canonical machine identifier
// (persisted in audit rows and HTML data-field attributes), Tier is one
// of TierGreen / TierYellow / TierRed, LegalAnchor names the LGPD
// article the classification cites, and PromptForm describes how the
// field appears in the LLM prompt when opted-in (cleartext for Green,
// "[PII:X]" token for Yellow, never sent for Red).
//
// LabelPT is the operator-facing Portuguese label rendered next to the
// checkbox. Keep these short — the SE spec lists the verbatim copy in
// the lgpd-field-spec banner; UI labels here repeat for accessibility
// (screen-reader announcement) and do NOT replace the spec text.
type LGPDField struct {
	Name        string
	Tier        LGPDFieldTier
	LegalAnchor string
	LabelPT     string
	PromptForm  string
}

// LGPDFieldCatalog returns the closed allow/deny list for the F8 field
// selector. Order is deterministic: Green first, Yellow next, Red last
// — the UI renders them in the same order so screen-reader announcement
// stays consistent across renders.
//
// Adding a row is an SE + UXDesigner decision (see lgpd-field-spec §"Tier
// classification"). Adding a Red row never requires UI work; adding a
// Green or Yellow row needs both a domain update and a template column.
func LGPDFieldCatalog() []LGPDField {
	return []LGPDField{
		// 🟢 Green — always sent (cleartext).
		{Name: "display_name", Tier: TierGreen, LegalAnchor: "LGPD Art. 5 I", LabelPT: "Nome do contato", PromptForm: `customer.display_name = "{name}"`},
		{Name: "tags", Tier: TierGreen, LegalAnchor: "LGPD Art. 5 I (minimização)", LabelPT: "Tags públicas", PromptForm: `customer.tags = [...]`},
		{Name: "channel", Tier: TierGreen, LegalAnchor: "não-pessoal", LabelPT: "Canal de origem", PromptForm: `customer.channel = "whatsapp"`},
		{Name: "conversation_summary_last_5", Tier: TierGreen, LegalAnchor: "LGPD Art. 5 I (derivado)", LabelPT: "Resumo das 5 últimas mensagens", PromptForm: `customer.summary = "(resumo anônimo)"`},

		// 🟡 Yellow — PII; opt-in required; ALWAYS tokenised.
		{Name: "email", Tier: TierYellow, LegalAnchor: "LGPD Art. 5 I (PII)", LabelPT: "E-mail", PromptForm: `customer.email = "[PII:EMAIL]"`},
		{Name: "phone", Tier: TierYellow, LegalAnchor: "LGPD Art. 5 I (PII)", LabelPT: "Telefone", PromptForm: `customer.phone = "[PII:PHONE]"`},
		{Name: "cnpj", Tier: TierYellow, LegalAnchor: "LGPD Art. 5 I (identificador empresarial)", LabelPT: "CNPJ (PJ)", PromptForm: `customer.cnpj = "[PII:CNPJ]"`},

		// 🔴 Red — LGPD-blocked. Never selectable.
		{Name: "cpf", Tier: TierRed, LegalAnchor: "LGPD Art. 5 I (alto risco de re-identificação)", LabelPT: "CPF", PromptForm: "nunca enviado"},
		{Name: "address", Tier: TierRed, LegalAnchor: "LGPD Art. 5 I (localização)", LabelPT: "Endereço", PromptForm: "nunca enviado"},
		{Name: "health_data", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II d (sensível — saúde)", LabelPT: "Dados de saúde", PromptForm: "nunca enviado"},
		{Name: "racial_ethnic_origin", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II a (sensível)", LabelPT: "Origem racial/étnica", PromptForm: "nunca enviado"},
		{Name: "religious_belief", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II b (sensível)", LabelPT: "Convicção religiosa", PromptForm: "nunca enviado"},
		{Name: "political_opinion", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II c (sensível)", LabelPT: "Opinião política", PromptForm: "nunca enviado"},
		{Name: "union_affiliation", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II e (sensível)", LabelPT: "Filiação sindical", PromptForm: "nunca enviado"},
		{Name: "sexual_orientation_data", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II d (sensível)", LabelPT: "Vida sexual", PromptForm: "nunca enviado"},
		{Name: "biometric_data", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II f (sensível)", LabelPT: "Dados biométricos", PromptForm: "nunca enviado"},
		{Name: "genetic_data", Tier: TierRed, LegalAnchor: "LGPD Art. 5 II g (sensível)", LabelPT: "Dados genéticos", PromptForm: "nunca enviado"},
		{Name: "children_data", Tier: TierRed, LegalAnchor: "LGPD Art. 14 (titular menor)", LabelPT: "Dados de menores", PromptForm: "nunca enviado"},
	}
}

// LGPDFieldByName returns the catalog entry for name. The boolean reports
// whether the lookup matched; an unknown name returns the zero value so
// callers can treat it as "not in the closed allow/deny list".
func LGPDFieldByName(name string) (LGPDField, bool) {
	for _, f := range LGPDFieldCatalog() {
		if f.Name == name {
			return f, true
		}
	}
	return LGPDField{}, false
}

// LGPDYellowFieldNames returns just the Yellow tier field names. The
// banner trigger, the resolver's tokenised preview, and the migration
// 0118 backfill all need this slice.
func LGPDYellowFieldNames() []string {
	out := []string{}
	for _, f := range LGPDFieldCatalog() {
		if f.Tier == TierYellow {
			out = append(out, f.Name)
		}
	}
	return out
}

// LGPDRedFieldNames returns just the Red tier field names. The handler
// rejects any POST that names one of these in structured_fields.
func LGPDRedFieldNames() []string {
	out := []string{}
	for _, f := range LGPDFieldCatalog() {
		if f.Tier == TierRed {
			out = append(out, f.Name)
		}
	}
	return out
}

// ErrLGPDBlockedField is returned by ValidateStructuredFields when the
// caller tries to opt into a Red-tier field. The error carries the field
// name so the form re-render can highlight the offending checkbox.
type ErrLGPDBlockedField struct {
	Field string
}

func (e *ErrLGPDBlockedField) Error() string {
	return fmt.Sprintf("aipolicy: field %q blocked by LGPD", e.Field)
}

// ErrUnknownStructuredField is returned by ValidateStructuredFields when
// the caller tries to opt into a name absent from LGPDFieldCatalog. The
// closed allow-list is deliberate: a future ADR adds the row before the
// UI exposes it.
type ErrUnknownStructuredField struct {
	Field string
}

func (e *ErrUnknownStructuredField) Error() string {
	return fmt.Sprintf("aipolicy: unknown structured field %q", e.Field)
}

// ValidateStructuredFields rejects Red-tier and unknown names, dedupes,
// and returns the canonical (sorted) Yellow subset. Green names are
// silently dropped because they are unconditional and never persisted in
// Policy.StructuredFields. Empty input is valid (the all-Yellow-OFF
// state) and returns []string{} — never nil — so DiffPolicies and the
// adapter can rely on the empty-slice invariant.
//
// errors.As(err, &lgpdErr) recovers the offending field name for the
// form-level re-render.
func ValidateStructuredFields(in []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := []string{}
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		entry, ok := LGPDFieldByName(name)
		if !ok {
			return nil, &ErrUnknownStructuredField{Field: name}
		}
		switch entry.Tier {
		case TierGreen:
			// Green is unconditional; the persisted slice only
			// captures explicit Yellow consent.
			continue
		case TierRed:
			return nil, &ErrLGPDBlockedField{Field: name}
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// EqualStructuredFields reports whether two field-sets carry the same
// names, ignoring order. Used by DiffPolicies so a re-save that does not
// change the set produces zero audit events.
func EqualStructuredFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// AnyYellowEnabled reports whether the field-set has at least one
// Yellow entry. The banner-render predicate in templates consults this
// to decide whether to emit the sticky LGPD inline alert.
func AnyYellowEnabled(fields []string) bool {
	yellow := map[string]struct{}{}
	for _, name := range LGPDYellowFieldNames() {
		yellow[name] = struct{}{}
	}
	for _, name := range fields {
		if _, ok := yellow[name]; ok {
			return true
		}
	}
	return false
}

// ContainsField reports whether name is in fields. The handler uses it
// for checkbox-state rendering: a Yellow entry whose name is in
// Policy.StructuredFields renders checked, otherwise unchecked.
func ContainsField(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// errLGPDBlockedSentinel is the umbrella sentinel a caller can match
// with errors.Is when they care that *any* Red field was attempted, not
// the specific name. ErrLGPDBlockedField wraps this so the typed
// payload remains accessible via errors.As.
var errLGPDBlockedSentinel = errors.New("aipolicy: lgpd-blocked structured field")

// Is matches the wrapped sentinel so errors.Is works for callers that
// want the umbrella check.
func (e *ErrLGPDBlockedField) Is(target error) bool { return target == errLGPDBlockedSentinel }

// ErrLGPDBlocked is the umbrella sentinel — errors.Is(err, ErrLGPDBlocked)
// is true for any *ErrLGPDBlockedField.
var ErrLGPDBlocked = errLGPDBlockedSentinel
