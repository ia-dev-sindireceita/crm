package aiassist

import (
	"errors"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// ErrConsentRequired is the sentinel returned when the operator has
// not yet accepted the anonymized payload preview for the scope the
// call is acting under, OR has accepted under a different anonymizer
// or prompt version. Callers branch on this with errors.Is and extract
// the preview + scope via errors.As against ConsentRequired.
//
// SIN-62928 / Fase 3 decisão #8. The sentinel lives in package
// aiassist (not in usecase/) so the future Suggest use-case (W4D) can
// reuse it without duplicating the type.
var ErrConsentRequired = errors.New("aiassist: consent required for scope")

// ConsentRequired is the rich error the gate returns alongside
// ErrConsentRequired. It carries everything the caller needs to
// render the confirmation UI: the anonymized preview the operator
// must inspect, the active versions the gate ran against, and the
// consent scope (tenant + kind + id) to be persisted on accept.
//
// errors.Is(err, ErrConsentRequired) is true via Unwrap; callers that
// need the preview use errors.As(err, &cr).
type ConsentRequired struct {
	// Scope identifies the (tenant, scope_kind, scope_id) triple the
	// gate hit. The web handler persists consent against this exact
	// scope when the operator accepts so the next call falls through
	// without re-prompting.
	Scope aipolicy.ConsentScope

	// Payload is the anonymized preview the operator MUST inspect.
	// The cleartext is never carried in this error; the field is
	// already PII-stripped by the gate's Anonymizer call.
	Payload string

	// AnonymizerVersion is the version of the anonymizer that
	// produced Payload. The web handler stores it on the consent row
	// so a future anonymizer roll-forward forces a re-consent.
	AnonymizerVersion string

	// PromptVersion is the active prompt template version at the
	// time of the gate. Same re-consent trigger as
	// AnonymizerVersion.
	PromptVersion string
}

// Error implements error. The message intentionally omits Payload —
// errors.Is + errors.As is the contract; logging the rendered error
// must not leak the (already anonymized but possibly revealing)
// preview into log lines.
func (e *ConsentRequired) Error() string { return ErrConsentRequired.Error() }

// Unwrap lets errors.Is(err, ErrConsentRequired) return true.
func (e *ConsentRequired) Unwrap() error { return ErrConsentRequired }
