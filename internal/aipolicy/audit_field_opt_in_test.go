package aipolicy_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// TestDiffPolicies_StructuredFields_Added pins SE regression test #4:
// toggling a Yellow field ON emits one ai_policy_audit row with
// Field="field_opt_in.<name>" and OldValue=false / NewValue=true.
func TestDiffPolicies_StructuredFields_Added(t *testing.T) {
	t.Parallel()
	prev := aipolicy.Policy{StructuredFields: []string{}}
	next := aipolicy.Policy{StructuredFields: []string{"email"}}
	events := aipolicy.DiffPolicies(prev, next, true)

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1; events=%+v", len(events), events)
	}
	ev := events[0]
	if ev.Field != aipolicy.FieldOptInName("email") {
		t.Fatalf("Field = %q, want %q", ev.Field, aipolicy.FieldOptInName("email"))
	}
	if !strings.HasPrefix(ev.Field, aipolicy.FieldOptInPrefix) {
		t.Fatalf("Field prefix mismatch: %q", ev.Field)
	}
	if ev.OldValue != false {
		t.Fatalf("OldValue = %v, want false", ev.OldValue)
	}
	if ev.NewValue != true {
		t.Fatalf("NewValue = %v, want true", ev.NewValue)
	}
}

// TestDiffPolicies_StructuredFields_Removed mirrors the above for
// Yellow → OFF transitions. The audit row carries OldValue=true,
// NewValue=false.
func TestDiffPolicies_StructuredFields_Removed(t *testing.T) {
	t.Parallel()
	prev := aipolicy.Policy{StructuredFields: []string{"email", "phone"}}
	next := aipolicy.Policy{StructuredFields: []string{"phone"}}
	events := aipolicy.DiffPolicies(prev, next, true)

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Field != aipolicy.FieldOptInName("email") {
		t.Fatalf("Field = %q, want %q", ev.Field, aipolicy.FieldOptInName("email"))
	}
	if ev.OldValue != true || ev.NewValue != false {
		t.Fatalf("(Old, New) = (%v, %v), want (true, false)", ev.OldValue, ev.NewValue)
	}
}

// TestDiffPolicies_StructuredFields_NoChange asserts a re-save with
// the same Yellow set emits ZERO field_opt_in events.
func TestDiffPolicies_StructuredFields_NoChange(t *testing.T) {
	t.Parallel()
	prev := aipolicy.Policy{StructuredFields: []string{"email", "phone"}}
	next := aipolicy.Policy{StructuredFields: []string{"phone", "email"}}
	events := aipolicy.DiffPolicies(prev, next, true)
	for _, ev := range events {
		if strings.HasPrefix(ev.Field, aipolicy.FieldOptInPrefix) {
			t.Fatalf("unexpected field_opt_in event on no-op: %+v", ev)
		}
	}
}

// TestDiffPolicies_StructuredFields_DeterministicOrder pins the
// sorted-by-name guarantee so snapshot assertions stay stable.
func TestDiffPolicies_StructuredFields_DeterministicOrder(t *testing.T) {
	t.Parallel()
	prev := aipolicy.Policy{StructuredFields: []string{}}
	next := aipolicy.Policy{StructuredFields: []string{"phone", "cnpj", "email"}}
	events := aipolicy.DiffPolicies(prev, next, true)
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}
	gotNames := []string{}
	for _, ev := range events {
		gotNames = append(gotNames, ev.Field)
	}
	wantNames := []string{
		aipolicy.FieldOptInName("cnpj"),
		aipolicy.FieldOptInName("email"),
		aipolicy.FieldOptInName("phone"),
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("event order = %v, want %v", gotNames, wantNames)
	}
}

// TestDiffPolicies_StructuredFields_CreatedHasSnapshot pins the
// FieldCreated lifecycle row: the NewValue snapshot includes
// structured_fields so the audit reader can reconstruct the initial
// state without replaying field_opt_in deltas.
func TestDiffPolicies_StructuredFields_CreatedHasSnapshot(t *testing.T) {
	t.Parallel()
	next := aipolicy.Policy{StructuredFields: []string{"email"}}
	events := aipolicy.DiffPolicies(aipolicy.Policy{}, next, false)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 created", len(events))
	}
	snap, ok := events[0].NewValue.(map[string]any)
	if !ok {
		t.Fatalf("NewValue type = %T, want map[string]any", events[0].NewValue)
	}
	fields, ok := snap["structured_fields"].([]string)
	if !ok {
		t.Fatalf("snapshot.structured_fields type = %T, want []string", snap["structured_fields"])
	}
	if !reflect.DeepEqual(fields, []string{"email"}) {
		t.Fatalf("snapshot.structured_fields = %v, want [email]", fields)
	}
}

// TestFieldOptInName pins the wire format so audit-table queries
// (LIKE 'field_opt_in.%') keep working.
func TestFieldOptInName(t *testing.T) {
	t.Parallel()
	got := aipolicy.FieldOptInName("email")
	want := "field_opt_in.email"
	if got != want {
		t.Fatalf("FieldOptInName = %q, want %q", got, want)
	}
}
