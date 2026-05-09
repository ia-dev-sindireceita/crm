package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// MFAAlerter is the Slack-incoming-webhook adapter for mfa.Alerter.
// It composes the existing Notifier (which already owns the webhook
// URL, timeout, and HTTP client) with a thin event-name layer so the
// rendered Slack message carries the master user-id, the event type,
// and the per-request operational context (ADR 0074 §5: actor +
// code_index + ip + user_agent + route).
//
// Compile-time assertion: MFAAlerter satisfies the domain port from
// internal/iam/mfa.
var _ mfa.Alerter = (*MFAAlerter)(nil)

// MFAAlerter routes mfa.Alerter calls to Slack via Notify.
type MFAAlerter struct {
	notifier *Notifier
}

// NewMFAAlerter returns the adapter. nil notifier is a programmer
// error and panics so a misconfigured wiring fails loudly.
func NewMFAAlerter(notifier *Notifier) *MFAAlerter {
	if notifier == nil {
		panic("slack: NewMFAAlerter: notifier is nil")
	}
	return &MFAAlerter{notifier: notifier}
}

// AlertRecoveryUsed posts the immediate Slack #alerts notification
// when a master recovery code is consumed (ADR 0074 §5).
//
// The message includes the literal event tag `master_recovery_used`
// so dashboards / SIEM pipelines that grep on event names see the
// same string the slog audit adapter writes, plus the four context
// fields the on-call operator needs to begin investigation:
// code_index, ip, user_agent, route.
func (a *MFAAlerter) AlertRecoveryUsed(ctx context.Context, details mfa.RecoveryUsedDetails) error {
	var b strings.Builder
	b.WriteString(":rotating_light: master_recovery_used")
	b.WriteString(" user=")
	b.WriteString(details.UserID.String())
	fmt.Fprintf(&b, " code_index=%d", details.CodeIndex)
	b.WriteString(" ip=")
	b.WriteString(formatField(details.IP))
	b.WriteString(" user_agent=")
	b.WriteString(formatField(details.UserAgent))
	b.WriteString(" route=")
	b.WriteString(formatField(details.Route))
	b.WriteString(" — investigate now")
	return a.notifier.Notify(ctx, b.String())
}

// AlertRecoveryRegenerated posts the Slack notification when a master
// regenerates the recovery code set (ADR 0074 §2). Less severe than
// AlertRecoveryUsed (intentional ops action vs. potential breach), so
// we use a different emoji to make the difference legible at a glance
// in the Slack channel. The same context fields ride along (sans
// code_index, which does not apply to regenerate) so an operator can
// confirm the regenerate was driven by the master themselves.
func (a *MFAAlerter) AlertRecoveryRegenerated(ctx context.Context, details mfa.RecoveryRegeneratedDetails) error {
	var b strings.Builder
	b.WriteString(":arrows_counterclockwise: master_recovery_regenerated")
	b.WriteString(" user=")
	b.WriteString(details.UserID.String())
	b.WriteString(" ip=")
	b.WriteString(formatField(details.IP))
	b.WriteString(" user_agent=")
	b.WriteString(formatField(details.UserAgent))
	b.WriteString(" route=")
	b.WriteString(formatField(details.Route))
	return a.notifier.Notify(ctx, b.String())
}

// formatField wraps a value in square brackets so a Slack reader can
// see field boundaries when the user-agent or route contains spaces.
// Empty values render as [] rather than disappearing — operators
// reading the alert can then attribute the gap to a missing header
// rather than a parsing bug in the alert pipeline. Brackets are also
// JSON-safe (unlike `"`) so the wire body stays human-readable when
// inspected as a raw webhook payload.
func formatField(v string) string {
	return "[" + v + "]"
}
