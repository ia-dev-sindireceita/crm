package whatsapp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
)

func TestTenantResolverFunc_Implements(t *testing.T) {
	t.Parallel()
	want := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	f := whatsapp.TenantResolverFunc(func(_ context.Context, pn string) (uuid.UUID, error) {
		if pn != "p1" {
			return uuid.Nil, whatsapp.ErrUnknownPhoneNumberID
		}
		return want, nil
	})
	got, err := f.Resolve(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := f.Resolve(context.Background(), "missing"); !errors.Is(err, whatsapp.ErrUnknownPhoneNumberID) {
		t.Fatalf("expected ErrUnknownPhoneNumberID, got %v", err)
	}
}

func TestNewTenantResolver_PanicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = whatsapp.NewTenantResolver(nil)
}

func TestNewTenantResolver_TranslatesUnknown(t *testing.T) {
	t.Parallel()
	lookup := &nopLookup{err: whatsapp.ErrAssociationUnknown}
	r := whatsapp.NewTenantResolver(lookup)
	if _, err := r.Resolve(context.Background(), "any"); !errors.Is(err, whatsapp.ErrUnknownPhoneNumberID) {
		t.Fatalf("expected ErrUnknownPhoneNumberID, got %v", err)
	}
}

func TestNewTenantResolver_TranslatesWrappedUnknown(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("lookup: %w", whatsapp.ErrAssociationUnknown)
	lookup := &nopLookup{err: wrapped}
	r := whatsapp.NewTenantResolver(lookup)
	if _, err := r.Resolve(context.Background(), "any"); !errors.Is(err, whatsapp.ErrUnknownPhoneNumberID) {
		t.Fatalf("expected ErrUnknownPhoneNumberID, got %v", err)
	}
}

func TestNewTenantResolver_PropagatesInfraError(t *testing.T) {
	t.Parallel()
	other := errors.New("connection refused")
	lookup := &nopLookup{err: other}
	r := whatsapp.NewTenantResolver(lookup)
	_, err := r.Resolve(context.Background(), "any")
	if err == nil || errors.Is(err, whatsapp.ErrUnknownPhoneNumberID) {
		t.Fatalf("expected wrapped infra error, got %v", err)
	}
	if !errors.Is(err, other) {
		t.Fatalf("error must wrap original via %%w, got %v", err)
	}
}

func TestNewTenantResolver_HappyPath(t *testing.T) {
	t.Parallel()
	want := uuid.MustParse("66666666-6666-4666-8666-666666666666")
	r := whatsapp.NewTenantResolver(&nopLookup{resp: want})
	got, err := r.Resolve(context.Background(), "any")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}
