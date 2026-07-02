package tenantusers

import (
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// now is overridable so unit tests can pin timestamps.
var now = func() time.Time { return time.Now().UTC() }

// User is the aggregate root for a tenant user managed via /settings/users.
// Identity is the UUID; tenant ownership is fixed at construction — a user
// cannot move between tenants. Only Role and Active are mutable, each
// through the use-case layer (Service) rather than directly on the
// aggregate, so the anti-escalation / anti-lockout invariants live in one
// place.
type User struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Email    string
	Role     iam.Role
	Active   bool
	// MustChangePassword marks a user seeded with a system-generated
	// temporary password. Forced-rotation-on-first-login enforcement is a
	// follow-up (SecurityEngineer sign-off); the flag is the storage seam.
	MustChangePassword bool
	CreatedAt          time.Time
	// PasswordHash is the encoded Argon2id hash. It is populated ONLY on the
	// create write path (New); Repository.List / Get leave it empty. The
	// hash is never rendered in any view and never logged.
	PasswordHash string
}

// RoleAssignable reports whether r is a role a gerente may assign through
// the surface. The set is closed to {tenant_gerente, tenant_atendente}:
// master (and every other value — tenant_common, tenant_lider, legacy,
// forged) is rejected. This is the single anti-escalation predicate the
// New constructor and Service.UpdateUserRole both consult.
func RoleAssignable(r iam.Role) bool {
	switch r {
	case iam.RoleTenantGerente, iam.RoleTenantAtendente:
		return true
	default:
		return false
	}
}

// NormalizeEmail lower-cases and trims the address. The users.email column
// is citext (case-insensitive unique per tenant); normalising here keeps
// the value we render and the value we compare consistent.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validEmail reports whether email parses as a single RFC 5322 address.
// mail.ParseAddress accepts display-name forms ("A <a@b>"); we reject those
// by requiring the parsed address to equal the (already-normalised) input.
func validEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return false
	}
	return strings.EqualFold(addr.Address, email)
}

// New builds a fresh, active tenant user seeded for forced password change.
// tenantID MUST be non-Nil; email MUST be a valid address; role MUST be
// assignable (anti-escalation); passwordHash MUST be non-empty. The user
// gets a freshly generated UUID, Active=true, MustChangePassword=true and
// CreatedAt=now(). is_master is not representable on the aggregate, so a
// user created here can never be a master.
func New(tenantID uuid.UUID, email string, role iam.Role, passwordHash string) (*User, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	email = NormalizeEmail(email)
	if !validEmail(email) {
		return nil, ErrInvalidEmail
	}
	if !RoleAssignable(role) {
		return nil, ErrRoleNotAssignable
	}
	if strings.TrimSpace(passwordHash) == "" {
		return nil, ErrEmptyPasswordHash
	}
	return &User{
		ID:                 uuid.New(),
		TenantID:           tenantID,
		Email:              email,
		Role:               role,
		Active:             true,
		MustChangePassword: true,
		CreatedAt:          now(),
		PasswordHash:       passwordHash,
	}, nil
}

// Hydrate rebuilds a User from stored fields without re-running New's
// invariants. Adapter code uses it to materialise rows read from Postgres;
// domain code MUST use New. PasswordHash is intentionally not a parameter —
// reads never carry the hash back into the domain.
func Hydrate(id, tenantID uuid.UUID, email string, role iam.Role, active, mustChangePassword bool, createdAt time.Time) *User {
	return &User{
		ID:                 id,
		TenantID:           tenantID,
		Email:              email,
		Role:               role,
		Active:             active,
		MustChangePassword: mustChangePassword,
		CreatedAt:          createdAt,
	}
}
