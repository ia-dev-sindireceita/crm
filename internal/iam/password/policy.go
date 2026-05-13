package password

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// ADR 0070 §5 — password policy bounds. NIST 800-63B-aligned: minimum
// length 12 chars, maximum 128 chars (truncation guard), no composition
// rules, no mandatory expiry, breach-corpus screening via HIBP +
// bundled top-100k local list.
const (
	minPolicyLength = 12
	maxPolicyLength = 128
)

// PolicyError names the first failed rule in a PolicyCheck so callers can
// render a localized message. The Reason field is a stable enum (never
// mutated for i18n purposes) so handler code can switch on it.
type PolicyError struct {
	Reason PolicyReason
	// Detail is a short English description for ops logs. It is NOT
	// rendered to end users — handlers map Reason to a localized copy.
	Detail string
}

func (e *PolicyError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("password policy: %s: %s", e.Reason, e.Detail)
}

// PolicyReason is the enum of policy failure categories. Stable across
// versions; new categories are additive.
type PolicyReason string

const (
	ReasonTooShort        PolicyReason = "too_short"
	ReasonTooLong         PolicyReason = "too_long"
	ReasonMatchesIdentity PolicyReason = "matches_identity"
	ReasonBreached        PolicyReason = "breached"
)

// Policy implements PolicyChecker.
//
// Pwned is the breach-corpus port (see ADR 0070 §5 fail-closed shape):
//   - If Pwned reports a hit, the password is rejected with ReasonBreached.
//   - If Pwned returns ErrPwnedCheckUnavailable, the policy MUST consult
//     LocalList; if the local list rejects, the password is breached;
//     if the local list accepts, the policy passes WITH a slog WARN
//     entry (event=iam_password_hibp_unavailable) so ops can see the
//     degraded-mode rate.
//   - Other Pwned errors (network noise, unexpected status) are treated
//     the same as ErrPwnedCheckUnavailable — fall through to LocalList.
//
// LocalList is the bundled top-100k checker. nil means "no local
// fallback" — degraded HIBP then yields a hard policy failure
// (ReasonBreached) so we never silently let a weak password through.
//
// Logger receives degraded-mode warnings; nil falls back to slog.Default.
type Policy struct {
	Pwned     PwnedPasswordChecker
	LocalList PwnedPasswordChecker
	Logger    *slog.Logger
}

// PolicyCheck implements PolicyChecker. It applies §5 in order: length
// bounds, identity equality, then breach-corpus screening. The first
// failing rule terminates the check.
func (p *Policy) PolicyCheck(ctx context.Context, plain string, pctx PolicyContext) error {
	// Length is measured in runes — a 12-rune password of `é` characters
	// is 24 UTF-8 bytes and must still pass the lower bound. Upper bound
	// is also rune-counted so an attacker cannot smuggle a megabyte of
	// emoji past the truncation guard.
	rl := utf8.RuneCountInString(plain)
	if rl < minPolicyLength {
		return &PolicyError{Reason: ReasonTooShort, Detail: fmt.Sprintf("min %d chars", minPolicyLength)}
	}
	if rl > maxPolicyLength {
		return &PolicyError{Reason: ReasonTooLong, Detail: fmt.Sprintf("max %d chars", maxPolicyLength)}
	}

	if matchesIdentity(plain, pctx) {
		return &PolicyError{Reason: ReasonMatchesIdentity, Detail: "must not equal email/username/tenant"}
	}

	if err := p.checkBreached(ctx, plain); err != nil {
		return err
	}
	return nil
}

func matchesIdentity(plain string, pctx PolicyContext) bool {
	low := strings.ToLower(strings.TrimSpace(plain))
	for _, candidate := range []string{pctx.Email, pctx.Username, pctx.TenantName} {
		c := strings.ToLower(strings.TrimSpace(candidate))
		if c == "" {
			continue
		}
		if c == low {
			return true
		}
	}
	return false
}

func (p *Policy) checkBreached(ctx context.Context, plain string) error {
	if p.Pwned == nil {
		// No remote checker wired — fall straight through to the local
		// list (or pass-through if neither is configured).
		return p.checkLocal(ctx, plain, false /* degraded */)
	}
	pwned, err := p.Pwned.IsPwned(ctx, plain)
	if err == nil {
		if pwned {
			return &PolicyError{Reason: ReasonBreached, Detail: "password appears in HIBP corpus"}
		}
		return nil
	}
	// Remote degraded — fall through to local list with a warning log.
	if !errors.Is(err, ErrPwnedCheckUnavailable) {
		// Treat unexpected errors the same as "unavailable" but log the
		// underlying cause so ops can investigate transient bugs.
		p.logger().WarnContext(ctx, "iam_password_hibp_error",
			slog.String("event", "iam_password_hibp_error"),
			slog.String("err", err.Error()),
		)
	}
	return p.checkLocal(ctx, plain, true /* degraded */)
}

func (p *Policy) checkLocal(ctx context.Context, plain string, degraded bool) error {
	if p.LocalList == nil {
		if degraded {
			// HIBP is down AND no local fallback — fail closed: the safer
			// posture is to reject than to pass an unscreened password.
			return &PolicyError{Reason: ReasonBreached, Detail: "breach-corpus unavailable; no local fallback"}
		}
		return nil
	}
	pwned, err := p.LocalList.IsPwned(ctx, plain)
	if err != nil {
		// Local list is bundled (embed.FS) and should never fail at
		// runtime; if it does, fail closed.
		return &PolicyError{Reason: ReasonBreached, Detail: "local breach list error"}
	}
	if pwned {
		return &PolicyError{Reason: ReasonBreached, Detail: "password in local top-100k list"}
	}
	if degraded {
		// HIBP unavailable but the local list passed. ADR §5 — pass with
		// a warning so the ops dashboard surfaces the rate.
		p.logger().WarnContext(ctx, "iam_password_hibp_unavailable",
			slog.String("event", "iam_password_hibp_unavailable"),
		)
	}
	return nil
}

func (p *Policy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
