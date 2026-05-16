// Package anonymizer is the pure-domain port for stripping Brazilian
// PII out of free-form text before it crosses the bounded-context
// boundary into an external LLM (OpenRouter today, anything tomorrow).
//
// The package is intentionally tiny: it declares the Anonymizer
// interface, the stable replacement tokens that consumers may pin
// their prompts to, and a sentinel error for the fail-closed
// contract. It imports only "context" — no regexp, no strings, no
// vendor SDK — so the domain stays implementation-free. Concrete
// adapters live under internal/ai-assist/anonymizer/*; see the regex
// sub-package for the production implementation.
//
// Product rule (ratified by the board in SIN-62203, ADR-0041):
// every payload that leaves for the OpenRouter adapter MUST first
// pass through an Anonymizer. The choice is opt-OUT only at the
// product level — tenants cannot disable it.
package anonymizer

import (
	"context"
	"errors"
)

// Stable replacement tokens. They are part of the public contract:
// downstream prompt templates and evaluation suites may pin against
// these literals, so changing them is a deliberate breaking change
// that MUST coincide with bumping the adapter's AnonymizerVersion.
const (
	// TokenPhone replaces Brazilian landline and mobile numbers
	// (10 or 11 digits, with or without DDD, DDI +55, parentheses,
	// spaces, or hyphens).
	TokenPhone = "[PII:PHONE]"
	// TokenEmail replaces email addresses, including plus-aliasing
	// and multi-label subdomains.
	TokenEmail = "[PII:EMAIL]"
	// TokenCPF replaces Brazilian individual taxpayer numbers,
	// formatted (XXX.XXX.XXX-XX) or raw (11 consecutive digits).
	TokenCPF = "[PII:CPF]"
	// TokenCNPJ replaces Brazilian company taxpayer numbers,
	// formatted (XX.XXX.XXX/XXXX-XX) or raw (14 consecutive digits).
	TokenCNPJ = "[PII:CNPJ]"
)

// ErrAnonymize is the sentinel returned when an adapter cannot
// guarantee that all PII patterns were considered (for example, a
// compiled pattern fails to apply or a downstream library panics
// and is recovered). It exists so callers can fail-closed without
// pattern-matching adapter-specific error strings: the use-case
// MUST abort the LLM call and surface an audible error to the
// operator rather than forward cleartext.
var ErrAnonymize = errors.New("ai-assist/anonymizer: anonymization failed")

// Anonymizer redacts Brazilian PII out of free-form text.
//
// Implementations MUST be:
//   - Idempotent — applying Anonymize twice produces the same
//     output as applying it once.
//   - Fail-closed — on any internal failure return ErrAnonymize (or
//     an error that wraps it) and an empty string. Never return the
//     original cleartext as a fallback.
//   - Side-effect free — no logging of input or output, since both
//     may carry pre-anonymisation PII. Adapters that need to log
//     MUST log only length or a hash digest.
//
// The empty string is a valid input and MUST round-trip unchanged.
//
// The contract deliberately does NOT redact proper names: that is a
// conversational signal the operator needs to keep, and any
// best-effort name detector is more likely to over-redact useful
// context than to protect anything LGPD already covers via the
// identifier classes above. Adding new PII classes is a versioned
// adapter change — see the package README for the procedure.
type Anonymizer interface {
	Anonymize(ctx context.Context, text string) (string, error)
}
