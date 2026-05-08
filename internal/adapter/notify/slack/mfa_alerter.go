package slack

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// MFAAlerter is the Slack-incoming-webhook adapter for mfa.Alerter.
// It composes the existing Notifier (which already owns the webhook
// URL, timeout, and HTTP client) with a thin event-name layer so the
// rendered Slack message carries the master user-id and the event
// type.
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
// same string the slog audit adapter writes.
func (a *MFAAlerter) AlertRecoveryUsed(ctx context.Context, userID uuid.UUID) error {
	msg := fmt.Sprintf(":rotating_light: master_recovery_used user=%s — investigate now", userID.String())
	return a.notifier.Notify(ctx, msg)
}

// AlertRecoveryRegenerated posts the Slack notification when a master
// regenerates the recovery code set (ADR 0074 §2). Less severe than
// AlertRecoveryUsed (intentional ops action vs. potential breach), so
// we use a different emoji to make the difference legible at a glance
// in the Slack channel.
func (a *MFAAlerter) AlertRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error {
	msg := fmt.Sprintf(":arrows_counterclockwise: master_recovery_regenerated user=%s", userID.String())
	return a.notifier.Notify(ctx, msg)
}
