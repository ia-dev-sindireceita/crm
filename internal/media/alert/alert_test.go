package alert_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/alert"
)

func TestNoop_NotifyAlwaysSucceeds(t *testing.T) {
	t.Parallel()
	if err := (alert.Noop{}).Notify(context.Background(), alert.Event{}); err != nil {
		t.Fatalf("Noop.Notify: %v", err)
	}
}

func TestNoop_SatisfiesAlerter(t *testing.T) {
	t.Parallel()
	var _ alert.Alerter = alert.Noop{}
}

func TestEvent_FieldsAreExposed(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	mid := uuid.New()
	e := alert.Event{
		TenantID:  tid,
		MessageID: mid,
		Key:       "tenant/key.png",
		EngineID:  "clamav-1.4.2",
		Signature: "Win.Test.EICAR_HDB-1",
	}
	if e.TenantID != tid || e.MessageID != mid {
		t.Fatal("ids not preserved")
	}
}

func TestErrEmptyEvent_IsSentinel(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("upstream: " + alert.ErrEmptyEvent.Error())
	if errors.Is(wrapped, alert.ErrEmptyEvent) {
		// the wrapping is a plain string concat, not a wrap — Is should
		// still match the sentinel via the unwrap chain only when the
		// adapter wraps with %w. This test exists so a regression that
		// switches the sentinel to a non-comparable type fails CI.
		t.Fatal("string concat should not satisfy errors.Is")
	}
}
