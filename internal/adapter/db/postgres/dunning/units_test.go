package dunning

// SIN-62965 — pure-Go unit tests for the constructor guards and the
// payload decoder. The DB-backed tests live in
// internal/adapter/db/postgres/dunning_adapter_test.go (postgres_test
// package) under the shared testpg harness.

import (
	"testing"
)

func TestNew_NilPools(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("New(nil,nil) returned nil error")
	}
}

func TestNewTickStore_NilArgs(t *testing.T) {
	if _, err := NewTickStore(nil, nil); err == nil {
		t.Fatal("NewTickStore(nil,nil) returned nil error")
	}
}

func TestNewCourtesyOverrideStore_NilPool(t *testing.T) {
	if _, err := NewCourtesyOverrideStore(nil); err == nil {
		t.Fatal("NewCourtesyOverrideStore(nil) returned nil error")
	}
}

func TestDecodeMonths(t *testing.T) {
	cases := []struct {
		name      string
		payload   []byte
		wantValue int
		wantOK    bool
	}{
		{"empty", nil, 0, false},
		{"not json", []byte("not-json"), 0, false},
		{"no months key", []byte(`{"plan_id":"x"}`), 0, false},
		{"valid float", []byte(`{"months":3}`), 3, true},
		{"zero", []byte(`{"months":0}`), 0, false},
		{"negative", []byte(`{"months":-1}`), 0, false},
		{"too big", []byte(`{"months":121}`), 0, false},
		{"non-numeric", []byte(`{"months":"three"}`), 0, false},
		{"max valid", []byte(`{"months":120}`), 120, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeMonths(tc.payload)
			if ok != tc.wantOK || got != tc.wantValue {
				t.Errorf("decodeMonths(%q) = (%d, %v), want (%d, %v)",
					tc.payload, got, ok, tc.wantValue, tc.wantOK)
			}
		})
	}
}
