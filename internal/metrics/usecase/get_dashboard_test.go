package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/metrics"
)

// fakeReader records the arguments Snapshot was called with and returns a
// canned result/error. It is an in-memory stand-in for the Postgres
// adapter — no database is mocked, only the port boundary.
type fakeReader struct {
	gotTenant uuid.UUID
	gotSince  time.Time
	called    bool
	result    metrics.DashboardMetrics
	err       error
}

func (f *fakeReader) Snapshot(_ context.Context, tenantID uuid.UUID, since time.Time) (metrics.DashboardMetrics, error) {
	f.called = true
	f.gotTenant = tenantID
	f.gotSince = since
	return f.result, f.err
}

func TestNewGetDashboard_RejectsNilReader(t *testing.T) {
	if _, err := NewGetDashboard(nil); !errors.Is(err, ErrNilReader) {
		t.Errorf("NewGetDashboard(nil) err = %v, want ErrNilReader", err)
	}
}

func TestNewGetDashboard_OK(t *testing.T) {
	uc, err := NewGetDashboard(&fakeReader{})
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	if uc == nil {
		t.Fatal("NewGetDashboard returned nil use case")
	}
}

func TestExecute_RejectsZeroTenant(t *testing.T) {
	fr := &fakeReader{}
	uc, err := NewGetDashboard(fr)
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	if _, err := uc.Execute(context.Background(), uuid.Nil, time.Time{}); err == nil {
		t.Error("Execute(uuid.Nil) err = nil, want validation error")
	}
	if fr.called {
		t.Error("reader called despite zero tenant; want short-circuit before query")
	}
}

func TestExecute_AppliesDefaultWindowOnZeroSince(t *testing.T) {
	fr := &fakeReader{}
	uc, err := NewGetDashboard(fr)
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	pinned := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	uc = uc.WithClock(func() time.Time { return pinned })

	tenant := uuid.New()
	if _, err := uc.Execute(context.Background(), tenant, time.Time{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fr.gotTenant != tenant {
		t.Errorf("reader tenant = %v, want %v", fr.gotTenant, tenant)
	}
	want := pinned.Add(-DefaultWindow)
	if !fr.gotSince.Equal(want) {
		t.Errorf("reader since = %v, want default window %v", fr.gotSince, want)
	}
}

func TestExecute_HonoursExplicitSince(t *testing.T) {
	fr := &fakeReader{}
	uc, err := NewGetDashboard(fr)
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	// A clock that would produce a very different default — proving the
	// explicit since wins.
	uc = uc.WithClock(func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) })

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if _, err := uc.Execute(context.Background(), uuid.New(), since); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !fr.gotSince.Equal(since) {
		t.Errorf("reader since = %v, want explicit %v", fr.gotSince, since)
	}
}

func TestExecute_ReturnsReaderResult(t *testing.T) {
	want := metrics.DashboardMetrics{
		ConversationsByState: []metrics.StateCount{{State: "open", Count: 3}},
		VolumeByChannel:      []metrics.ChannelCount{{Channel: "whatsapp", Count: 3}},
		FirstResponse:        metrics.Percentiles{P50: 5 * time.Second},
	}
	fr := &fakeReader{result: want}
	uc, err := NewGetDashboard(fr)
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	got, err := uc.Execute(context.Background(), uuid.New(), time.Now())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got.ConversationsByState) != 1 || got.ConversationsByState[0].Count != 3 {
		t.Errorf("ConversationsByState = %+v, want %+v", got.ConversationsByState, want.ConversationsByState)
	}
	if got.FirstResponse.P50 != 5*time.Second {
		t.Errorf("FirstResponse.P50 = %v, want 5s", got.FirstResponse.P50)
	}
}

func TestExecute_PropagatesReaderError(t *testing.T) {
	sentinel := errors.New("boom")
	fr := &fakeReader{err: sentinel}
	uc, err := NewGetDashboard(fr)
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}
	if _, err := uc.Execute(context.Background(), uuid.New(), time.Now()); !errors.Is(err, sentinel) {
		t.Errorf("Execute err = %v, want sentinel", err)
	}
}
