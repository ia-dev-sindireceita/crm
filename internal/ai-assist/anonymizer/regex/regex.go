// Package regex provides the production Anonymizer implementation
// backed by a small set of pre-compiled, linear regular expressions.
//
// The adapter realises the contract declared by the
// internal/ai-assist/anonymizer port. Every regex compiled at
// package init uses bounded quantifiers (\d{N} or \d{4,5}) so an
// adversarial input cannot trigger super-linear backtracking
// (ADR-0041 §"ReDoS"). The set of patterns is intentionally short
// and is NEVER exposed: it is encapsulated inside the package so
// callers cannot widen the redaction surface accidentally.
//
// Adapter wiring: cmd/server injects a *regex.Adapter as the
// anonymizer.Anonymizer the OpenRouter chat-completions client
// depends on. Tests in this package are pure unit tests with no
// database, network, or filesystem touch.
package regex

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pericles-luz/crm/internal/ai-assist/anonymizer"
)

// AnonymizerVersion is the contract version of the regex adapter.
// It is exposed so that callers writing the LLM prompt envelope can
// pin a known token-set + pattern revision. Bumping this constant
// is a deliberate breaking change: any prompt evaluation suite that
// asserts against [PII:*] placements MUST be re-baselined when the
// version moves forward.
const AnonymizerVersion = "v1"

var (
	// emailRE is intentionally tolerant: it accepts plus-aliasing
	// (foo+bar@x.y), dotted subdomains and short single-letter TLDs
	// (a@b.c.d — the issue spec calls this out explicitly), and the
	// ASCII punctuation that RFC 5321 allows in the local-part. We
	// do not try to be a full RFC parser — the goal is to redact,
	// not validate.
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]+`)

	// cnpjMaskedRE matches the canonical formatted CNPJ
	// "XX.XXX.XXX/XXXX-XX". The `/` makes this unambiguous and lets
	// it run before any other 14-digit detection.
	cnpjMaskedRE = regexp.MustCompile(`\b\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}\b`)

	// cpfMaskedRE matches the canonical formatted CPF
	// "XXX.XXX.XXX-XX".
	cpfMaskedRE = regexp.MustCompile(`\b\d{3}\.\d{3}\.\d{3}-\d{2}\b`)

	// cnpjRawRE matches exactly 14 consecutive digits flanked by
	// word boundaries. Real CNPJs are always 14 digits long; longer
	// runs are deliberately not matched because they are almost
	// always something else (transaction ids, opaque keys, etc.).
	cnpjRawRE = regexp.MustCompile(`\b\d{14}\b`)

	// cpfRawRE matches exactly 11 consecutive digits flanked by
	// word boundaries. By design this also redacts raw 11-digit
	// mobile phone numbers under the CPF token — see the README
	// for the rationale: both are PII, and forcing the input to
	// disambiguate would over-engineer the boundary case.
	cpfRawRE = regexp.MustCompile(`\b\d{11}\b`)

	// phoneRE matches Brazilian landline (10 digits incl. DDD) and
	// mobile (11 digits incl. DDD) numbers, with or without the +55
	// DDI prefix, with or without parentheses around the DDD, and
	// with or without spaces/hyphens between the groups. The
	// alternation in the DDD section forces the input to carry SOME
	// signal — a "+55" prefix, parentheses, or an explicit space
	// or hyphen between DDD and the rest — which is what keeps a
	// bare 8-digit run from being matched as a phone (the explicit
	// false-positive case named in the issue acceptance criteria).
	phoneRE = regexp.MustCompile(`(?:\+?55[ \-]?\(?\d{2}\)?[ \-]?|\(\d{2}\)[ \-]?|\d{2}[ \-])9?\d{4}[ \-]?\d{4}`)
)

// Option configures an Adapter at construction time. Options compose
// — see WithCPFChecksum for the canonical example.
type Option func(*Adapter)

// WithCPFChecksum toggles strict mod-11 validation of matched CPFs.
// When enabled, a substring that LOOKS like a CPF but fails the
// checksum is left intact in the output. This is a defence in depth
// against false positives such as run-length identifiers that
// happen to fit \d{11} or the masked pattern; it is OFF by default
// because in conversational text every 11-digit number is almost
// certainly PII regardless of checksum.
func WithCPFChecksum(enabled bool) Option {
	return func(a *Adapter) { a.cpfChecksum = enabled }
}

// Adapter is the regex-backed Anonymizer. It is safe for concurrent
// use: it holds only immutable configuration (the compiled regexes
// are package-level globals).
type Adapter struct {
	cpfChecksum bool
}

// New builds an Adapter and applies the supplied options in order.
// The zero-option call returns the default configuration: CPF
// checksum OFF, all four PII classes redacted.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Anonymize implements anonymizer.Anonymizer. The empty string is
// passed through unchanged. Any internal panic — defensive only;
// the regex package and the pure-string callbacks below should not
// panic on any input — is recovered and surfaced as ErrAnonymize so
// the caller fails closed and never forwards cleartext.
func (a *Adapter) Anonymize(ctx context.Context, text string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = ""
			err = fmt.Errorf("%w: %v", anonymizer.ErrAnonymize, r)
		}
	}()

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: %w", anonymizer.ErrAnonymize, err)
	}
	if text == "" {
		return "", nil
	}

	// Order matters: the most specific patterns run first so that
	// generic raw-digit patterns cannot eat substrings that belong
	// to a more structured class.
	text = emailRE.ReplaceAllString(text, anonymizer.TokenEmail)
	text = cnpjMaskedRE.ReplaceAllString(text, anonymizer.TokenCNPJ)
	text = a.replaceCPFMasked(text)
	text = cnpjRawRE.ReplaceAllString(text, anonymizer.TokenCNPJ)
	text = a.replaceCPFRaw(text)
	text = phoneRE.ReplaceAllString(text, anonymizer.TokenPhone)

	return text, nil
}

// replaceCPFMasked runs the formatted-CPF replacement, optionally
// gated by mod-11 validation. Centralising the gate here keeps the
// "checksum on / checksum off" branching out of Anonymize.
func (a *Adapter) replaceCPFMasked(text string) string {
	if !a.cpfChecksum {
		return cpfMaskedRE.ReplaceAllString(text, anonymizer.TokenCPF)
	}
	return cpfMaskedRE.ReplaceAllStringFunc(text, func(match string) string {
		if validCPF(match) {
			return anonymizer.TokenCPF
		}
		return match
	})
}

// replaceCPFRaw runs the raw-11-digit replacement, optionally gated
// by mod-11 validation. Mirrors replaceCPFMasked.
func (a *Adapter) replaceCPFRaw(text string) string {
	if !a.cpfChecksum {
		return cpfRawRE.ReplaceAllString(text, anonymizer.TokenCPF)
	}
	return cpfRawRE.ReplaceAllStringFunc(text, func(match string) string {
		if validCPF(match) {
			return anonymizer.TokenCPF
		}
		return match
	})
}

// validCPF returns true when s — after non-digit characters are
// stripped — is an 11-digit Brazilian CPF whose two check digits
// satisfy the mod-11 rule. The exact algorithm is the Receita
// Federal specification: weighted sums, modulo 11, with the
// remainder mapped to zero when it is ten.
//
// Callers in this package always feed validCPF the matched substring
// of cpfMaskedRE or cpfRawRE; both guarantee onlyDigits will return
// exactly 11 bytes, so the function does not re-check length.
func validCPF(s string) bool {
	digits := onlyDigits(s)
	var sum int
	for i := 0; i < 9; i++ {
		sum += int(digits[i]-'0') * (10 - i)
	}
	d1 := (sum * 10) % 11
	if d1 == 10 {
		d1 = 0
	}
	if int(digits[9]-'0') != d1 {
		return false
	}
	sum = 0
	for i := 0; i < 10; i++ {
		sum += int(digits[i]-'0') * (11 - i)
	}
	d2 := (sum * 10) % 11
	if d2 == 10 {
		d2 = 0
	}
	return int(digits[10]-'0') == d2
}

// onlyDigits returns s with every non-digit byte removed. The
// helper exists so the checksum routine can accept both raw and
// masked CPF strings without conditional branching on the format.
func onlyDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// compile-time guard: Adapter implements the port.
var _ anonymizer.Anonymizer = (*Adapter)(nil)
