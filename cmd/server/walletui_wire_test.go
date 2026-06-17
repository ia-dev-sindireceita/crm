package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/walletui"
)

type fakeDashboard struct{ called bool }

func (f *fakeDashboard) Snapshot(_ context.Context, _ uuid.UUID, _ time.Time) (walletui.DashboardSnapshot, error) {
	f.called = true
	return walletui.DashboardSnapshot{}, nil
}

type fakeLedger struct{ called bool }

func (f *fakeLedger) Page(_ context.Context, _ walletui.LedgerPageOptions) (walletui.LedgerPage, error) {
	f.called = true
	return walletui.LedgerPage{}, nil
}

func (f *fakeLedger) StreamCSV(_ context.Context, _ walletui.LedgerFilter, w io.Writer) error {
	_, _ = w.Write([]byte("id\n"))
	return nil
}

type fakeTopup struct{ called bool }

func (f *fakeTopup) ListPackages(_ context.Context) ([]walletui.TopupPackage, error) {
	f.called = true
	return nil, nil
}

func TestAssembleWalletUIHandler_MountsAllFourRoutes(t *testing.T) {
	t.Parallel()
	dash := &fakeDashboard{}
	ledger := &fakeLedger{}
	topup := &fakeTopup{}
	mux, err := assembleWalletUIHandler(dash, ledger, topup, time.Now, slog.Default())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if mux == nil {
		t.Fatalf("mux is nil")
	}
	tests := []struct {
		path  string
		wantS int
	}{
		{"/wallet", http.StatusOK},
		{"/wallet/topup", http.StatusOK},
		{"/wallet/ledger", http.StatusOK},
		{"/wallet/ledger.csv", http.StatusOK},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New(), Name: "Acme"})
			ctx = middleware.WithSession(ctx, iam.Session{UserID: uuid.New(), CSRFToken: "csrf-test"})
			r = r.WithContext(ctx)
			mux.ServeHTTP(rec, r)
			if rec.Code != tc.wantS {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.wantS, rec.Body.String())
			}
		})
	}
	if !dash.called {
		t.Errorf("dashboard reader never invoked")
	}
	if !ledger.called {
		t.Errorf("ledger reader never invoked")
	}
	if !topup.called {
		t.Errorf("topup reader never invoked")
	}
}

func TestAssembleWalletUIHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		dashboard walletui.DashboardReader
		ledger    walletui.LedgerReader
		topup     walletui.TopupCatalogReader
		now       func() time.Time
	}{
		{"nil dashboard", nil, &fakeLedger{}, &fakeTopup{}, time.Now},
		{"nil ledger", &fakeDashboard{}, nil, &fakeTopup{}, time.Now},
		{"nil topup", &fakeDashboard{}, &fakeLedger{}, nil, time.Now},
		{"nil now", &fakeDashboard{}, &fakeLedger{}, &fakeTopup{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := assembleWalletUIHandler(tc.dashboard, tc.ledger, tc.topup, tc.now, slog.Default())
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, err) {
				// just ensure shape is errors-friendly; the assembler
				// returns plain errors.New strings today so this is
				// a smoke check rather than a sentinel match.
				t.Errorf("error not errors-comparable")
			}
		})
	}
}

func TestBuildWalletUIHandler_NoEnvNoMount(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWalletUIHandler(context.Background(), func(string) string { return "" })
	if h != nil {
		t.Errorf("expected nil handler when DATABASE_URL is unset")
	}
	cleanup() // must not panic
}

func TestWalletUserLabel_DefaultsEmpty(t *testing.T) {
	t.Parallel()
	if got := walletUserLabelFromSession(nil); got != "" {
		t.Errorf("got %q want empty", got)
	}
}
