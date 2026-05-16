package noop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/notify/email/noop"
	"github.com/pericles-luz/crm/internal/notify/email"
)

func TestNoop_SatisfiesPort(t *testing.T) {
	t.Parallel()
	var _ email.EmailSender = noop.New()
}

func TestNoop_SendValidMessageReturnsNil(t *testing.T) {
	t.Parallel()
	s := noop.New()
	msg := email.Message{
		From:    email.Address{Email: "noreply@acme.com"},
		To:      []email.Address{{Email: "alice@example.com"}},
		Subject: "x",
		Text:    "y",
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestNoop_SendInvalidMessageReturnsValidationError(t *testing.T) {
	t.Parallel()
	s := noop.New()
	err := s.Send(context.Background(), email.Message{})
	if err == nil {
		t.Fatal("Send returned nil for invalid message")
	}
	if !errors.Is(err, email.ErrInvalidMessage) {
		t.Fatalf("Send err = %v, does not wrap ErrInvalidMessage", err)
	}
}
