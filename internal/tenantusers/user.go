package tenantusers

import (
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// User is the tenant-scoped user aggregate as seen by the management surface.
// It intentionally carries no password material — credentials live behind the
// invite Token flow and the login path.
type User struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Email         string
	Role          iam.Role
	DeactivatedAt *time.Time
	CreatedAt     time.Time
}

// Active reports whether the user may authenticate (not soft-deactivated).
func (u User) Active() bool { return u.DeactivatedAt == nil }

// Actor is the authenticated gerente performing an operation. TenantID and
// UserID are ALWAYS derived from the session (SIN-66494 G2) — never from
// client input. UserID drives audit actor attribution and self-detection.
type Actor struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
}

// Domain errors. Callers map these to HTTP status at the boundary:
//   - ErrInvalidEmail  → 422
//   - ErrInvalidRole   → 422 (also the deny-by-default guard against master)
//   - ErrEmailTaken    → 422
//   - ErrUserNotFound  → 404
//   - ErrLastGerente   → 409 (anti-lockout)
var (
	ErrInvalidEmail = errors.New("tenantusers: invalid email")
	ErrInvalidRole  = errors.New("tenantusers: role is not assignable by a gerente")
	ErrEmailTaken   = errors.New("tenantusers: email already exists in this tenant")
	ErrUserNotFound = errors.New("tenantusers: user not found")
	ErrLastGerente  = errors.New("tenantusers: cannot demote or deactivate the last active gerente")
)

// AssignableRoles is the G1 allowlist: the only roles a tenant gerente may
// assign on create or role-change. This is an allowlist (deny-by-default),
// NOT a denylist of "master" — any value outside this set is rejected.
func AssignableRoles() []iam.Role {
	return []iam.Role{iam.RoleTenantGerente, iam.RoleTenantAtendente}
}

// AssignableRole reports whether r may be assigned by a gerente.
func AssignableRole(r iam.Role) bool {
	return r == iam.RoleTenantGerente || r == iam.RoleTenantAtendente
}

// normalizeEmail trims surrounding space. The column is citext so case is
// folded at the storage layer; we do not lower-case here to preserve the
// address the gerente typed for display.
func normalizeEmail(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(e)
	if err != nil {
		return "", ErrInvalidEmail
	}
	// ParseAddress accepts "Name <a@b>"; require a bare address.
	if addr.Name != "" || addr.Address != e {
		return "", ErrInvalidEmail
	}
	return addr.Address, nil
}
