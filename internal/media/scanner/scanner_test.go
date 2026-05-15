package scanner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/media/scanner"
)

// fakeScanner is an in-process MediaScanner used to exercise the port
// contract. Trivial on purpose — no goroutines, no network, no shared
// state — so the test exercises the shape of the interface, not
// transport behaviour. Concrete adapters ([SIN-62788] F2-05b ClamAV)
// must satisfy this same contract.
type fakeScanner struct {
	result  scanner.ScanResult
	err     error
	lastKey string
}

func (f *fakeScanner) Scan(_ context.Context, key string) (scanner.ScanResult, error) {
	f.lastKey = key
	if f.err != nil {
		return scanner.ScanResult{}, f.err
	}
	return f.result, nil
}

// Compile-time proof the fake satisfies the port. If the port drifts,
// this assignment breaks the build before the test runs.
var _ scanner.MediaScanner = (*fakeScanner)(nil)

func TestMediaScannerContract(t *testing.T) {
	t.Parallel()
	errIO := errors.New("scanner: simulated I/O failure (engine connection refused)")
	const key = "media/tenant-uuid/2026-05/abc123.png"

	cases := []struct {
		name    string
		fake    *fakeScanner
		wantRes scanner.ScanResult
		wantErr error
	}{
		{
			name:    "pending → clean",
			fake:    &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "fake-1.0"}},
			wantRes: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "fake-1.0"},
		},
		{
			name:    "pending → infected",
			fake:    &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "fake-1.0"}},
			wantRes: scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "fake-1.0"},
		},
		{
			name:    "I/O error propagates",
			fake:    &fakeScanner{err: errIO},
			wantErr: errIO,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s scanner.MediaScanner = tc.fake
			got, err := s.Scan(context.Background(), key)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Scan err = %v, want %v", err, tc.wantErr)
				}
				if got != (scanner.ScanResult{}) {
					t.Fatalf("Scan result on error = %+v, want zero", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan err = %v, want nil", err)
			}
			if got != tc.wantRes {
				t.Fatalf("Scan result = %+v, want %+v", got, tc.wantRes)
			}
			if tc.fake.lastKey != key {
				t.Fatalf("fake.lastKey = %q, want %q (port must pass key through unchanged)", tc.fake.lastKey, key)
			}
		})
	}
}

func TestStatusValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    scanner.Status
		want bool
	}{
		{scanner.StatusPending, true},
		{scanner.StatusClean, true},
		{scanner.StatusInfected, true},
		{scanner.Status(""), false},
		{scanner.Status("quarantined"), false},
	}
	for _, tc := range cases {
		if got := tc.s.Valid(); got != tc.want {
			t.Errorf("Status(%q).Valid() = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// Pin the wire/JSON representation. Migration 0092 documents these
// exact strings in `message.media -> scan_status`; the adapter and
// worker rely on them. Any drift here is a schema break.
func TestStatusValuesAreStableStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got  scanner.Status
		want string
	}{
		{scanner.StatusPending, "pending"},
		{scanner.StatusClean, "clean"},
		{scanner.StatusInfected, "infected"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("status string = %q, want %q (migration 0092 contract)", tc.got, tc.want)
		}
	}
}
