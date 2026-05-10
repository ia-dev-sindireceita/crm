package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// VerifyLockoutAlerter is the Slack-incoming-webhook adapter for
// mastermfa.LockoutAlerter (SIN-62380 / CAVEAT-3 of SIN-62343). It
// composes the existing Notifier (which already owns the webhook
// URL, timeout, and HTTP client) with a thin event-name layer so the
// rendered Slack message carries the literal `master_2fa_verify_lockout`
// tag plus the operational context (user_id, session_id, failure
// count, ip, user_agent, route).
//
// Compile-time assertion: VerifyLockoutAlerter satisfies the domain
// port from internal/adapter/httpapi/mastermfa.
var _ mastermfa.LockoutAlerter = (*VerifyLockoutAlerter)(nil)

// VerifyLockoutAlerter routes mastermfa.LockoutAlerter calls to Slack
// via Notify.
type VerifyLockoutAlerter struct {
	notifier *Notifier
}

// NewVerifyLockoutAlerter returns the adapter. nil notifier is a
// programmer error and panics so a misconfigured wireup fails loudly
// (consistent with NewMFAAlerter shape).
func NewVerifyLockoutAlerter(notifier *Notifier) *VerifyLockoutAlerter {
	if notifier == nil {
		panic("slack: NewVerifyLockoutAlerter: notifier is nil")
	}
	return &VerifyLockoutAlerter{notifier: notifier}
}

// AlertVerifyLockout posts the immediate Slack #alerts notification
// when the master 2FA verify failure counter trips (ADR 0074 §6).
//
// The message includes the literal event tag
// `master_2fa_verify_lockout` so dashboards / SIEM pipelines that
// grep on event names see the same string the slog warn line writes
// (verify.go / tripLockout). The four operational fields the on-call
// operator needs to start triage are bracketed for human readability
// (matching the MFAAlerter format) and joined into one line so a
// single Slack post carries everything: user / session / failure
// count / ip / user_agent / route.
func (a *VerifyLockoutAlerter) AlertVerifyLockout(ctx context.Context, details mastermfa.VerifyLockoutDetails) error {
	var b strings.Builder
	b.WriteString(":lock: master_2fa_verify_lockout")
	b.WriteString(" user=")
	b.WriteString(details.UserID.String())
	b.WriteString(" session=")
	b.WriteString(details.SessionID.String())
	fmt.Fprintf(&b, " failures=%d", details.Failures)
	b.WriteString(" ip=")
	b.WriteString(formatField(details.IP))
	b.WriteString(" user_agent=")
	b.WriteString(formatField(details.UserAgent))
	b.WriteString(" route=")
	b.WriteString(formatField(details.Route))
	b.WriteString(" — session invalidated; investigate now")
	return a.notifier.Notify(ctx, b.String())
}
