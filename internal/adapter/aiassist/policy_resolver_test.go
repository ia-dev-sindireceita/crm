package aiassistadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// fakeResolver is a hand-rolled aipolicyResolver: it records the input
// it was handed and replays a canned (Policy, source, err) so the
// bridge's translation can be asserted without Postgres.
type fakeResolver struct {
	gotInput aipolicy.ResolveInput
	policy   aipolicy.Policy
	source   aipolicy.ResolveSource
	err      error
}

func (f *fakeResolver) Resolve(_ context.Context, in aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error) {
	f.gotInput = in
	return f.policy, f.source, f.err
}

func TestNewPolicyResolver_RejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := NewPolicyResolver(nil); err == nil {
		t.Fatal("NewPolicyResolver(nil): expected error, got nil")
	}
}

func TestPolicyResolver_TranslatesFields(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	fake := &fakeResolver{
		policy: aipolicy.Policy{
			Model:            "x-ai/grok",
			PromptVersion:    "v3",
			AIEnabled:        true,
			Anonymize:        true,
			OptIn:            true,
			StructuredFields: []string{"customer.email"},
		},
		source: aipolicy.SourceChannel,
	}
	bridge, err := NewPolicyResolver(fake)
	if err != nil {
		t.Fatalf("NewPolicyResolver: %v", err)
	}

	got, err := bridge.Resolve(context.Background(), tenant, aiassist.Scope{
		ChannelID: "whatsapp",
		TeamID:    "vendas",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := aiassist.Policy{
		AIEnabled:        true,
		OptIn:            true,
		Anonymize:        true,
		Model:            "x-ai/grok",
		MaxOutputTokens:  0,
		PromptVersion:    "v3",
		StructuredFields: []string{"customer.email"},
	}
	if got.AIEnabled != want.AIEnabled || got.OptIn != want.OptIn ||
		got.Anonymize != want.Anonymize || got.Model != want.Model ||
		got.MaxOutputTokens != want.MaxOutputTokens || got.PromptVersion != want.PromptVersion {
		t.Fatalf("Resolve scalar mismatch:\n got %+v\nwant %+v", got, want)
	}
	if len(got.StructuredFields) != 1 || got.StructuredFields[0] != "customer.email" {
		t.Fatalf("StructuredFields = %v; want [customer.email]", got.StructuredFields)
	}

	// Scope translation: non-empty strings become non-nil pointers.
	if fake.gotInput.TenantID != tenant {
		t.Fatalf("TenantID = %v; want %v", fake.gotInput.TenantID, tenant)
	}
	if fake.gotInput.ChannelID == nil || *fake.gotInput.ChannelID != "whatsapp" {
		t.Fatalf("ChannelID = %v; want pointer to whatsapp", fake.gotInput.ChannelID)
	}
	if fake.gotInput.TeamID == nil || *fake.gotInput.TeamID != "vendas" {
		t.Fatalf("TeamID = %v; want pointer to vendas", fake.gotInput.TeamID)
	}
}

func TestPolicyResolver_EmptyScopeBecomesNilPointers(t *testing.T) {
	t.Parallel()
	fake := &fakeResolver{policy: aipolicy.DefaultPolicy(), source: aipolicy.SourceDefault}
	bridge, err := NewPolicyResolver(fake)
	if err != nil {
		t.Fatalf("NewPolicyResolver: %v", err)
	}

	if _, err := bridge.Resolve(context.Background(), uuid.New(), aiassist.Scope{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fake.gotInput.ChannelID != nil {
		t.Fatalf("empty ChannelID must map to nil pointer, got %v", *fake.gotInput.ChannelID)
	}
	if fake.gotInput.TeamID != nil {
		t.Fatalf("empty TeamID must map to nil pointer, got %v", *fake.gotInput.TeamID)
	}
}

func TestPolicyResolver_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("resolve boom")
	fake := &fakeResolver{err: sentinel}
	bridge, err := NewPolicyResolver(fake)
	if err != nil {
		t.Fatalf("NewPolicyResolver: %v", err)
	}

	_, err = bridge.Resolve(context.Background(), uuid.New(), aiassist.Scope{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want errors.Is(err, sentinel)", err)
	}
}
