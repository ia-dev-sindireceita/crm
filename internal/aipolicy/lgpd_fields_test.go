package aipolicy_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// TestLGPDFieldCatalog_TiersExpected pins the SE-authored field tier
// classification (lgpd-field-spec §"Tier classification"). The closed
// allow/deny list is the contract every downstream consumer trusts; a
// silent change here cascades to the form, the audit reader, and the
// migration backfill.
func TestLGPDFieldCatalog_TiersExpected(t *testing.T) {
	t.Parallel()
	want := map[string]aipolicy.LGPDFieldTier{
		"display_name":                aipolicy.TierGreen,
		"tags":                        aipolicy.TierGreen,
		"channel":                     aipolicy.TierGreen,
		"conversation_summary_last_5": aipolicy.TierGreen,
		"email":                       aipolicy.TierYellow,
		"phone":                       aipolicy.TierYellow,
		"cnpj":                        aipolicy.TierYellow,
		"cpf":                         aipolicy.TierRed,
		"address":                     aipolicy.TierRed,
		"health_data":                 aipolicy.TierRed,
		"racial_ethnic_origin":        aipolicy.TierRed,
		"religious_belief":            aipolicy.TierRed,
		"political_opinion":           aipolicy.TierRed,
		"union_affiliation":           aipolicy.TierRed,
		"sexual_orientation_data":     aipolicy.TierRed,
		"biometric_data":              aipolicy.TierRed,
		"genetic_data":                aipolicy.TierRed,
		"children_data":               aipolicy.TierRed,
	}
	for _, entry := range aipolicy.LGPDFieldCatalog() {
		got, ok := want[entry.Name]
		if !ok {
			t.Errorf("catalog includes unexpected field %q", entry.Name)
			continue
		}
		if entry.Tier != got {
			t.Errorf("field %q tier = %q, want %q", entry.Name, entry.Tier, got)
		}
		delete(want, entry.Name)
	}
	if len(want) > 0 {
		t.Errorf("catalog missing expected fields: %v", keysOf(want))
	}
}

func keysOf(m map[string]aipolicy.LGPDFieldTier) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLGPDFieldCatalog_CPFIsRed pins the SE spec's "CPF is Red, not
// Yellow" decision. A regression here means the structured-field
// selector exposed cleartext (or even tokenized) CPF, contradicting
// the minimization argument in lgpd-field-spec.
func TestLGPDFieldCatalog_CPFIsRed(t *testing.T) {
	t.Parallel()
	got, ok := aipolicy.LGPDFieldByName("cpf")
	if !ok {
		t.Fatalf("cpf missing from catalog")
	}
	if got.Tier != aipolicy.TierRed {
		t.Fatalf("cpf tier = %q, want Red", got.Tier)
	}
}

// TestValidateStructuredFields_RejectsRed pins SE regression test #1:
// any POST that includes a Red field returns an ErrLGPDBlockedField.
func TestValidateStructuredFields_RejectsRed(t *testing.T) {
	t.Parallel()
	for _, name := range aipolicy.LGPDRedFieldNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := aipolicy.ValidateStructuredFields([]string{name})
			if err == nil {
				t.Fatalf("ValidateStructuredFields(%q) err = nil, want LGPD block", name)
			}
			var blocked *aipolicy.ErrLGPDBlockedField
			if !errors.As(err, &blocked) {
				t.Fatalf("err type = %T, want *ErrLGPDBlockedField", err)
			}
			if blocked.Field != name {
				t.Fatalf("blocked.Field = %q, want %q", blocked.Field, name)
			}
			if !errors.Is(err, aipolicy.ErrLGPDBlocked) {
				t.Fatalf("errors.Is(err, ErrLGPDBlocked) = false")
			}
		})
	}
}

// TestValidateStructuredFields_AcceptsYellow pins the Yellow opt-in
// path: every Yellow field validates and round-trips back as the
// canonical (sorted, deduped) Yellow subset.
func TestValidateStructuredFields_AcceptsYellow(t *testing.T) {
	t.Parallel()
	in := []string{"phone", "email", "email", "cnpj", ""}
	got, err := aipolicy.ValidateStructuredFields(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"cnpj", "email", "phone"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestValidateStructuredFields_RejectsUnknown checks the closed
// allow/deny list: a name absent from LGPDFieldCatalog is rejected.
func TestValidateStructuredFields_RejectsUnknown(t *testing.T) {
	t.Parallel()
	_, err := aipolicy.ValidateStructuredFields([]string{"surprise_field"})
	if err == nil {
		t.Fatalf("err = nil, want unknown field rejection")
	}
	var unknown *aipolicy.ErrUnknownStructuredField
	if !errors.As(err, &unknown) {
		t.Fatalf("err type = %T, want *ErrUnknownStructuredField", err)
	}
	if unknown.Field != "surprise_field" {
		t.Fatalf("unknown.Field = %q, want %q", unknown.Field, "surprise_field")
	}
}

// TestValidateStructuredFields_DropsGreen pins the contract that
// Green entries are unconditional and NOT persisted in
// Policy.StructuredFields — they're always sent regardless.
func TestValidateStructuredFields_DropsGreen(t *testing.T) {
	t.Parallel()
	got, err := aipolicy.ValidateStructuredFields([]string{"display_name", "email", "tags"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(got, []string{"email"}) {
		t.Fatalf("got %v, want [email]", got)
	}
}

// TestValidateStructuredFields_EmptyReturnsEmpty exercises the
// happy-path "all Yellow OFF" default (SE regression test #6).
func TestValidateStructuredFields_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	got, err := aipolicy.ValidateStructuredFields(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatalf("got nil, want non-nil empty slice (downstream invariant)")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want []", got)
	}
}

// TestAnyYellowEnabled covers the banner-trigger predicate. The form
// renders the sticky inline LGPD banner iff at least one Yellow field
// is opted-in (SE regression test #2).
func TestAnyYellowEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"empty", nil, false},
		{"green only (unreachable post-validate)", []string{"display_name"}, false},
		{"one yellow", []string{"email"}, true},
		{"all yellow", []string{"email", "phone", "cnpj"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aipolicy.AnyYellowEnabled(tc.in); got != tc.want {
				t.Errorf("AnyYellowEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestContainsField covers the per-field checkbox-state predicate.
func TestContainsField(t *testing.T) {
	t.Parallel()
	if !aipolicy.ContainsField([]string{"email", "phone"}, "email") {
		t.Errorf("ContainsField missed an existing entry")
	}
	if aipolicy.ContainsField([]string{"phone"}, "email") {
		t.Errorf("ContainsField false-positive for absent entry")
	}
}

// TestEqualStructuredFields covers the diff predicate that suppresses
// no-op audit events when a re-save keeps the same field set.
func TestEqualStructuredFields(t *testing.T) {
	t.Parallel()
	if !aipolicy.EqualStructuredFields([]string{"a", "b"}, []string{"b", "a"}) {
		t.Errorf("expected order-insensitive equality")
	}
	if aipolicy.EqualStructuredFields([]string{"a"}, []string{"a", "b"}) {
		t.Errorf("expected length mismatch to fail equality")
	}
	if aipolicy.EqualStructuredFields([]string{"a"}, []string{"b"}) {
		t.Errorf("expected different-set to fail equality")
	}
}

// TestLGPDYellowFieldNames_Stable pins the Yellow allow-list. The
// migration 0118 backfill uses ARRAY['email','phone','cnpj']; any
// drift here breaks the backfill assumption.
func TestLGPDYellowFieldNames_Stable(t *testing.T) {
	t.Parallel()
	want := []string{"email", "phone", "cnpj"}
	got := aipolicy.LGPDYellowFieldNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Yellow names = %v, want %v", got, want)
	}
}

// TestErrLGPDBlockedField_Error pins the typed error's message shape.
func TestErrLGPDBlockedField_Error(t *testing.T) {
	t.Parallel()
	err := &aipolicy.ErrLGPDBlockedField{Field: "cpf"}
	if !strings.Contains(err.Error(), "cpf") {
		t.Fatalf("Error() = %q, want field name in message", err.Error())
	}
}

// TestErrUnknownStructuredField_Error pins the typed error's message shape.
func TestErrUnknownStructuredField_Error(t *testing.T) {
	t.Parallel()
	err := &aipolicy.ErrUnknownStructuredField{Field: "surprise"}
	if !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("Error() = %q, want field name in message", err.Error())
	}
}
