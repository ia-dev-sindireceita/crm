package instagram_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

func TestTenantResolverFunc_DelegatesToClosure(t *testing.T) {
	t.Parallel()
	want := uuid.New()
	r := instagram.TenantResolverFunc(func(_ context.Context, _ string) (uuid.UUID, error) {
		return want, nil
	})
	got, err := r.Resolve(context.Background(), "any")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("Resolve: got %s, want %s", got, want)
	}
}

func TestNewTenantResolver_NilLookupPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil lookup")
		}
	}()
	instagram.NewTenantResolver(nil)
}

func TestPgTenantResolver_TranslatesAssociationUnknown(t *testing.T) {
	t.Parallel()
	r := instagram.NewTenantResolver(&nopLookup{err: instagram.ErrAssociationUnknown})
	_, err := r.Resolve(context.Background(), "igb-1")
	if !errors.Is(err, instagram.ErrUnknownIGBusinessID) {
		t.Fatalf("expected ErrUnknownIGBusinessID, got %v", err)
	}
}

func TestPgTenantResolver_WrapsOtherErrors(t *testing.T) {
	t.Parallel()
	r := instagram.NewTenantResolver(&nopLookup{err: errInjected})
	_, err := r.Resolve(context.Background(), "igb-1")
	if err == nil || errors.Is(err, instagram.ErrUnknownIGBusinessID) {
		t.Fatalf("expected wrapped non-sentinel error, got %v", err)
	}
}

func TestPgTenantResolver_ReturnsTenantOnHit(t *testing.T) {
	t.Parallel()
	want := uuid.New()
	r := instagram.NewTenantResolver(&nopLookup{resp: want})
	got, err := r.Resolve(context.Background(), "igb-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("Resolve: got %s, want %s", got, want)
	}
}
