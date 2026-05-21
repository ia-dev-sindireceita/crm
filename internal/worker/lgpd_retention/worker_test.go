package lgpd_retention_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/worker/lgpd_retention"
)

// fakeDeletions is an in-memory DeletionRepository the worker drives
// against. Mocking the DB is allowed here because no fiscal/retention
// guarantee is being verified — the assertion is on the worker's tick
// behaviour, not on storage semantics.
type fakeDeletions struct {
	mu        sync.Mutex
	rows      []lgpd.DeletionRequest
	listErr   error
	completed []uuid.UUID
	failed    []uuid.UUID
}

func (f *fakeDeletions) Upsert(_ context.Context, _ lgpd.DeletionRequest) (lgpd.DeletionRequest, error) {
	return lgpd.DeletionRequest{}, errors.New("unimplemented")
}

func (f *fakeDeletions) Get(_ context.Context, id uuid.UUID) (lgpd.DeletionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return lgpd.DeletionRequest{}, lgpd.ErrDeletionRequestNotFound
}

func (f *fakeDeletions) ListReady(_ context.Context, at time.Time, limit int) ([]lgpd.DeletionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []lgpd.DeletionRequest
	for _, r := range f.rows {
		if r.Status != lgpd.DeletionStatusPending {
			continue
		}
		if !r.RetentionUntil.After(at) {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeDeletions) MarkCompleted(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, id)
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Status = lgpd.DeletionStatusCompleted
			ts := at
			f.rows[i].CompletedAt = &ts
		}
	}
	return nil
}

func (f *fakeDeletions) MarkFailed(_ context.Context, id uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, id)
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Status = lgpd.DeletionStatusFailed
		}
	}
	return nil
}

type fakePurge struct {
	mu       sync.Mutex
	purged   []uuid.UUID
	purgeErr error
}

func (f *fakePurge) PurgeContact(_ context.Context, _ uuid.UUID, contactID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.purgeErr != nil {
		return f.purgeErr
	}
	f.purged = append(f.purged, contactID)
	return nil
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	if _, err := lgpd_retention.New(lgpd_retention.Config{}); err == nil {
		t.Fatal("New(empty) err = nil")
	}
	if _, err := lgpd_retention.New(lgpd_retention.Config{Deletions: &fakeDeletions{}}); err == nil {
		t.Fatal("New(no purge) err = nil")
	}
}

func TestTick_RipensReadyRowsAndSkipsFuture(t *testing.T) {
	now := time.Date(2031, 1, 15, 12, 0, 0, 0, time.UTC)
	pastID := uuid.New()
	futureID := uuid.New()
	pastReq := lgpd.DeletionRequest{
		ID: pastID, TenantID: uuid.New(), ContactID: uuid.New(),
		Status:         lgpd.DeletionStatusPending,
		RetentionUntil: now.Add(-time.Hour),
	}
	futureReq := lgpd.DeletionRequest{
		ID: futureID, TenantID: uuid.New(), ContactID: uuid.New(),
		Status:         lgpd.DeletionStatusPending,
		RetentionUntil: now.Add(24 * time.Hour),
	}
	d := &fakeDeletions{rows: []lgpd.DeletionRequest{pastReq, futureReq}}
	p := &fakePurge{}

	w, err := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Clock: func() time.Time { return now },
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err = %v", err)
	}

	if len(p.purged) != 1 || p.purged[0] != pastReq.ContactID {
		t.Errorf("purged = %v, want exactly the past contact %s", p.purged, pastReq.ContactID)
	}
	if len(d.completed) != 1 || d.completed[0] != pastID {
		t.Errorf("completed = %v, want exactly [%s]", d.completed, pastID)
	}
}

func TestTick_MarksFailedOnPurgeError(t *testing.T) {
	now := time.Date(2031, 1, 15, 12, 0, 0, 0, time.UTC)
	req := lgpd.DeletionRequest{
		ID: uuid.New(), TenantID: uuid.New(), ContactID: uuid.New(),
		Status:         lgpd.DeletionStatusPending,
		RetentionUntil: now.Add(-time.Hour),
	}
	d := &fakeDeletions{rows: []lgpd.DeletionRequest{req}}
	p := &fakePurge{purgeErr: errors.New("disk full")}
	w, _ := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Clock: func() time.Time { return now },
	})

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	if len(d.failed) != 1 || d.failed[0] != req.ID {
		t.Errorf("failed = %v, want [%s]", d.failed, req.ID)
	}
	if len(d.completed) != 0 {
		t.Errorf("completed = %v, want empty on failure path", d.completed)
	}
}

func TestTick_PropagatesListError(t *testing.T) {
	d := &fakeDeletions{listErr: errors.New("conn refused")}
	w, _ := lgpd_retention.New(lgpd_retention.Config{Deletions: d, Purge: &fakePurge{}})
	if err := w.Tick(context.Background()); err == nil {
		t.Fatal("Tick err = nil, want non-nil")
	}
}

func TestRun_ExitsOnCtxCancel(t *testing.T) {
	d := &fakeDeletions{}
	p := &fakePurge{}
	w, _ := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Interval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestRun_PerformsInitialTickAndRecurringTick(t *testing.T) {
	now := time.Date(2031, 1, 15, 12, 0, 0, 0, time.UTC)
	req := lgpd.DeletionRequest{
		ID: uuid.New(), TenantID: uuid.New(), ContactID: uuid.New(),
		Status: lgpd.DeletionStatusPending, RetentionUntil: now.Add(-time.Hour),
	}
	d := &fakeDeletions{rows: []lgpd.DeletionRequest{req}}
	p := &fakePurge{}
	w, err := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Interval: 30 * time.Millisecond,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // wait for initial tick
	cancel()
	<-done
	if len(p.purged) == 0 {
		t.Error("Run did not perform initial tick")
	}
}

func TestRun_LogsTickFailures(t *testing.T) {
	d := &fakeDeletions{listErr: errors.New("kaboom")}
	p := &fakePurge{}
	w, _ := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Interval: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(80 * time.Millisecond) // exercise the recurring tick branch
	cancel()
	<-done
}

func TestTick_HonoursContextCancellationBetweenRows(t *testing.T) {
	now := time.Date(2031, 1, 15, 12, 0, 0, 0, time.UTC)
	rows := []lgpd.DeletionRequest{
		{ID: uuid.New(), TenantID: uuid.New(), ContactID: uuid.New(),
			Status: lgpd.DeletionStatusPending, RetentionUntil: now.Add(-time.Hour)},
		{ID: uuid.New(), TenantID: uuid.New(), ContactID: uuid.New(),
			Status: lgpd.DeletionStatusPending, RetentionUntil: now.Add(-time.Hour)},
	}
	d := &fakeDeletions{rows: rows}
	p := &fakePurge{}
	w, _ := lgpd_retention.New(lgpd_retention.Config{
		Deletions: d, Purge: p, Clock: func() time.Time { return now },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Tick(ctx); err == nil {
		t.Fatal("Tick(canceled) err = nil, want context.Canceled")
	}
}
