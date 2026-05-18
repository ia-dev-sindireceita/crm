package postgres_test

// SIN-62965 — pure-Go unit coverage for the dunning adapter
// constructors and the payload decoder. The DB-backed tests live in
// dunning_adapter_test.go; these stay here in the parent postgres_test
// package because SIN-62750 forbids subpackage *_test.go under
// internal/adapter/db/postgres/* (shared CI cluster ALTER ROLE race).

import (
	"errors"
	"testing"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	dunningpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/dunning"
)

func TestDunningStore_New_RejectsNilPools(t *testing.T) {
	t.Parallel()
	if _, err := dunningpg.New(nil, nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("New(nil,nil) err = %v, want ErrNilPool", err)
	}
}

func TestDunningTickStore_New_RejectsNilArgs(t *testing.T) {
	t.Parallel()
	if _, err := dunningpg.NewTickStore(nil, nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("NewTickStore(nil,nil) err = %v, want ErrNilPool", err)
	}
}

func TestDunningCourtesyOverrideStore_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := dunningpg.NewCourtesyOverrideStore(nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("NewCourtesyOverrideStore(nil) err = %v, want ErrNilPool", err)
	}
}

func TestDunningDecodeMonths(t *testing.T) {
	t.Parallel()
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := dunningpg.DecodeMonths(tc.payload)
			if ok != tc.wantOK || got != tc.wantValue {
				t.Errorf("DecodeMonths(%q) = (%d, %v), want (%d, %v)",
					tc.payload, got, ok, tc.wantValue, tc.wantOK)
			}
		})
	}
}
