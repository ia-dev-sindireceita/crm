package postgres_test

// SIN-63224 follow-up to SIN-63222: bring the postgres adapter package
// above the 85% coverage bar by exercising master_directory.go, which
// landed without tests when SIN-62342 wired master MFA.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

func TestNewMasterDirectory_RejectsBadInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pool    *pgxpool.Pool
		actor   uuid.UUID
		wantErr error
	}{
		{name: "nil pool", pool: nil, actor: uuid.New(), wantErr: postgres.ErrNilPool},
		{name: "zero actor", pool: &pgxpool.Pool{}, actor: uuid.Nil, wantErr: postgres.ErrZeroActor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := postgres.NewMasterDirectory(tc.pool, tc.actor)
			if d != nil {
				t.Fatalf("got non-nil directory %v, want nil on validation error", d)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestMasterDirectory_EmailFor(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	actor := seedMasterUser(t, db, "ops-actor@master-dir.test")
	target := seedMasterUser(t, db, "ops-target@master-dir.test")

	d, err := postgres.NewMasterDirectory(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterDirectory: %v", err)
	}
	ctx := context.Background()

	t.Run("returns email of existing master user", func(t *testing.T) {
		got, err := d.EmailFor(ctx, target)
		if err != nil {
			t.Fatalf("EmailFor: %v", err)
		}
		if got != "ops-target@master-dir.test" {
			t.Fatalf("email = %q, want %q", got, "ops-target@master-dir.test")
		}
	})

	t.Run("missing row wraps not-found error", func(t *testing.T) {
		missing := uuid.New()
		_, err := d.EmailFor(ctx, missing)
		if err == nil {
			t.Fatalf("expected error for missing user")
		}
		// EmailFor returns a wrapped pgx.ErrNoRows with a descriptive
		// message containing the user id. We don't expose a sentinel
		// for the not-found case, so assert on the message shape.
		if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), missing.String()) {
			t.Fatalf("err = %v, want message containing 'not found' and %s", err, missing)
		}
	})

	t.Run("tenant user is invisible to master directory", func(t *testing.T) {
		// MasterDirectory filters on is_master = true; a tenant user
		// must surface as not-found even though the id exists in
		// the users table.
		tenant, tenantUser := seedTenantUser(t, db, "acme-mdir.crm.local", "agent@acme-mdir.test")
		_ = tenant
		_, err := d.EmailFor(ctx, tenantUser)
		if err == nil {
			t.Fatalf("expected error: tenant user must not surface in MasterDirectory")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v, want 'not found'", err)
		}
	})
}
