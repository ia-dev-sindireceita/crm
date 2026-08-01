package nats_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/media/worker"
)

type fakeMediaScanTarget struct {
	subject string
	body    []byte
	err     error
	calls   int
}

func (f *fakeMediaScanTarget) Publish(_ context.Context, subject string, body []byte) error {
	f.calls++
	f.subject = subject
	f.body = body
	return f.err
}

func TestNewMediaScanRequestPublisher_RejectsNilTarget(t *testing.T) {
	t.Parallel()
	if _, err := natsadapter.NewMediaScanRequestPublisher(nil); err == nil {
		t.Fatal("expected error on nil target")
	}
}

func TestMediaScanRequestPublisher_PublishesOnRequestedSubject(t *testing.T) {
	t.Parallel()
	target := &fakeMediaScanTarget{}
	p, err := natsadapter.NewMediaScanRequestPublisher(target)
	if err != nil {
		t.Fatalf("NewMediaScanRequestPublisher: %v", err)
	}

	tenantID := uuid.New()
	messageID := uuid.New()
	if err := p.PublishScanRequest(context.Background(), tenantID, messageID, "tenant/instagram/mid/image/0"); err != nil {
		t.Fatalf("PublishScanRequest: %v", err)
	}

	if target.calls != 1 {
		t.Fatalf("expected 1 publish call, got %d", target.calls)
	}
	if target.subject != worker.SubjectRequested {
		t.Errorf("subject: got %q, want %q", target.subject, worker.SubjectRequested)
	}
	var got worker.Request
	if err := json.Unmarshal(target.body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.TenantID != tenantID || got.MessageID != messageID || got.Key != "tenant/instagram/mid/image/0" {
		t.Errorf("body round-trip mismatch: got %+v", got)
	}
}

func TestMediaScanRequestPublisher_WrapsTargetError(t *testing.T) {
	t.Parallel()
	boom := errors.New("nats down")
	target := &fakeMediaScanTarget{err: boom}
	p, err := natsadapter.NewMediaScanRequestPublisher(target)
	if err != nil {
		t.Fatalf("NewMediaScanRequestPublisher: %v", err)
	}
	err = p.PublishScanRequest(context.Background(), uuid.New(), uuid.New(), "k")
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped %v, got %v", boom, err)
	}
}
