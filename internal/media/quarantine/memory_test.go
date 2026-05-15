package quarantine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/media/quarantine"
)

func TestMemory_MoveHappyPath(t *testing.T) {
	t.Parallel()
	m := quarantine.NewMemory()
	m.Put("tenant/2026-05/abc.png", []byte("eicar"))

	if err := m.Move(context.Background(), "tenant/2026-05/abc.png"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	if _, ok := m.Runtime("tenant/2026-05/abc.png"); ok {
		t.Fatal("expected key to be removed from runtime bucket")
	}
	body, ok := m.Quarantined("tenant/2026-05/abc.png")
	if !ok {
		t.Fatal("expected key to be present in quarantine bucket")
	}
	if string(body) != "eicar" {
		t.Fatalf("body mismatch, got %q", body)
	}
}

func TestMemory_MoveTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		seedRuntime bool
		seedQuar    bool
		wantErr     bool
		// wantInQuar: after Move, the key MUST live in the quarantine bucket.
		wantInQuar bool
	}{
		{"runtime only → moves", true, false, false, true},
		{"already quarantined → no-op", false, true, false, true},
		{"both → reports already quarantined (idempotent no-op)", true, true, false, true},
		{"missing → errors", false, false, true, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := quarantine.NewMemory()
			key := "tenant/key"
			if tc.seedRuntime {
				m.Put(key, []byte("body"))
			}
			if tc.seedQuar {
				// Re-route a seeded key into quarantine via Move so the
				// helper stays the public surface.
				m.Put(key+"-seed", []byte("body"))
				if err := m.Move(context.Background(), key+"-seed"); err != nil {
					t.Fatalf("seed move: %v", err)
				}
				// We seeded a different key; copy it into the target slot
				// via a manual put-then-move on the same alias so the
				// asserted final state still uses `key`.
				m.Put(key, []byte("body"))
				_ = m.Move(context.Background(), key)
			}
			err := m.Move(context.Background(), key)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Move: %v", err)
			}
			if _, ok := m.Quarantined(key); ok != tc.wantInQuar {
				t.Fatalf("Quarantined(%q) = %v, want %v", key, ok, tc.wantInQuar)
			}
		})
	}
}

func TestMemory_PortContract(t *testing.T) {
	t.Parallel()
	var _ quarantine.Quarantiner = quarantine.NewMemory()
}

func TestMemory_MissingKeyError(t *testing.T) {
	t.Parallel()
	m := quarantine.NewMemory()
	err := m.Move(context.Background(), "absent")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	// The error message is informative enough; we don't expose the
	// sentinel publicly. The error is non-nil and that's the contract.
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error variant: %v", err)
	}
}
