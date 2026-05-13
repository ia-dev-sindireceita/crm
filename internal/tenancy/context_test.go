package tenancy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
)

func TestContext_RoundTrip(t *testing.T) {
	t.Parallel()

	want := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	ctx := tenancy.WithContext(context.Background(), want)

	got, err := tenancy.FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext returned err: %v", err)
	}
	if got != want {
		t.Fatalf("FromContext returned %#v, want %#v", got, want)
	}
}

func TestContext_MissingReturnsErr(t *testing.T) {
	t.Parallel()

	if _, err := tenancy.FromContext(context.Background()); !errors.Is(err, tenancy.ErrNoTenantInContext) {
		t.Fatalf("FromContext err = %v, want ErrNoTenantInContext", err)
	}
}

func TestContext_NilTenantReturnsErr(t *testing.T) {
	t.Parallel()

	ctx := tenancy.WithContext(context.Background(), nil)
	if _, err := tenancy.FromContext(ctx); !errors.Is(err, tenancy.ErrNoTenantInContext) {
		t.Fatalf("FromContext nil err = %v, want ErrNoTenantInContext", err)
	}
}
