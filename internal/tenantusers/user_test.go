package tenantusers_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

func TestRoleAssignable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role iam.Role
		want bool
	}{
		{iam.RoleTenantGerente, true},
		{iam.RoleTenantAtendente, true},
		{iam.RoleTenantCommon, false},
		{iam.RoleTenantLider, false},
		{iam.RoleMaster, false},
		{iam.Role("is_master"), false},
		{iam.Role("admin"), false},
		{iam.Role(""), false},
		{iam.Role("garbage"), false},
	}
	for _, c := range cases {
		if got := tenantusers.RoleAssignable(c.role); got != c.want {
			t.Errorf("RoleAssignable(%q) = %v, want %v", c.role, got, c.want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	if got := tenantusers.NormalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Fatalf("NormalizeEmail = %q, want alice@example.com", got)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		u, err := tenantusers.New(tenant, "Op@Acme.com", iam.RoleTenantAtendente, "argon2id$hash")
		if err != nil {
			t.Fatalf("New err = %v", err)
		}
		if u.Email != "op@acme.com" {
			t.Errorf("email = %q, want normalised", u.Email)
		}
		if u.ID == uuid.Nil {
			t.Error("expected a generated id")
		}
		if !u.Active {
			t.Error("new user must be active")
		}
		if !u.MustChangePassword {
			t.Error("new user must be seeded for forced rotation")
		}
		if u.PasswordHash != "argon2id$hash" {
			t.Errorf("hash = %q, want carried through", u.PasswordHash)
		}
		if u.CreatedAt.IsZero() {
			t.Error("CreatedAt must be set")
		}
	})

	t.Run("nil tenant", func(t *testing.T) {
		t.Parallel()
		if _, err := tenantusers.New(uuid.Nil, "a@b.com", iam.RoleTenantGerente, "h"); err != tenantusers.ErrInvalidTenant {
			t.Fatalf("err = %v, want ErrInvalidTenant", err)
		}
	})

	t.Run("bad email", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"", "not-an-email", "a@", "@b.com", "a b@c.com"} {
			if _, err := tenantusers.New(tenant, bad, iam.RoleTenantGerente, "h"); err != tenantusers.ErrInvalidEmail {
				t.Errorf("New(%q) err = %v, want ErrInvalidEmail", bad, err)
			}
		}
	})

	t.Run("anti-escalation: non-assignable role", func(t *testing.T) {
		t.Parallel()
		for _, r := range []iam.Role{iam.RoleMaster, iam.RoleTenantCommon, iam.Role("is_master"), iam.Role("garbage")} {
			if _, err := tenantusers.New(tenant, "a@b.com", r, "h"); err != tenantusers.ErrRoleNotAssignable {
				t.Errorf("New(role=%q) err = %v, want ErrRoleNotAssignable", r, err)
			}
		}
	})

	t.Run("empty hash", func(t *testing.T) {
		t.Parallel()
		if _, err := tenantusers.New(tenant, "a@b.com", iam.RoleTenantGerente, "   "); err != tenantusers.ErrEmptyPasswordHash {
			t.Fatalf("err = %v, want ErrEmptyPasswordHash", err)
		}
	})
}

func TestGenerateTempPassword(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		pw, err := tenantusers.GenerateTempPassword()
		if err != nil {
			t.Fatalf("GenerateTempPassword err = %v", err)
		}
		if len(pw) < 16 {
			t.Fatalf("temp password too short: %d", len(pw))
		}
		if _, dup := seen[pw]; dup {
			t.Fatalf("duplicate temp password generated: %q", pw)
		}
		seen[pw] = struct{}{}
		// No ambiguous glyphs.
		if strings.ContainsAny(pw, "0O1lI") {
			t.Fatalf("temp password contains ambiguous glyph: %q", pw)
		}
	}
}
