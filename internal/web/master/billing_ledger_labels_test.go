package master_test

// SIN-63987 — raise internal/web/master coverage above the >85% bar.
//
// These table-driven tests pin every switch branch of the small pure
// ledger-label helpers (ledgerSourceLabel / ledgerKindLabel) plus the
// isZeroUUID template predicate. They were rendering at 42–66% (only
// the branches exercised incidentally by the billing handler tests);
// this drives them to full statement coverage without touching any
// existing test.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/master"
)

func TestLedgerSourceLabel_AllBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"monthly_alloc", "Alocação mensal"},
		{"master_grant", "Master grant"},
		{"consumption", "Consumo"},
		{"", "—"},
		{"something_unknown", "something_unknown"}, // default: render raw
	}
	for _, c := range cases {
		if got := master.ExportLedgerSourceLabel(c.in); got != c.want {
			t.Errorf("ledgerSourceLabel(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestLedgerKindLabel_AllBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"reserve", "Reserva"},
		{"commit", "Commit"},
		{"release", "Release"},
		{"grant", "Grant"},
		{"", "—"},
		{"weird_kind", "weird_kind"}, // default: render raw
	}
	for _, c := range cases {
		if got := master.ExportLedgerKindLabel(c.in); got != c.want {
			t.Errorf("ledgerKindLabel(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsZeroUUID(t *testing.T) {
	t.Parallel()
	if !master.ExportIsZeroUUID(uuid.Nil) {
		t.Error("isZeroUUID(uuid.Nil)=false, want true")
	}
	if master.ExportIsZeroUUID(uuid.MustParse("11111111-1111-1111-1111-111111111111")) {
		t.Error("isZeroUUID(non-zero)=true, want false")
	}
}
