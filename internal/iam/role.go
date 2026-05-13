package iam

// Role identifies the kind of session principal. ADR 0073 D3 tunes the
// idle/hard timeouts per Role; D4 also keys the rate-limit/lockout policy
// off it. Master is the operator console; the three tenant-* values are
// per-customer roles.
//
// The string values are persisted in session.role and read back on every
// request, so do NOT rename them once shipped — add new values instead.
type Role string

// Role values. ADR 0073 D3 lists exactly these four; future roles need an
// ADR delta because they require new idle/hard pairs.
const (
	RoleMaster          Role = "master"
	RoleTenantGerente   Role = "tenant_gerente"
	RoleTenantAtendente Role = "tenant_atendente"
	RoleTenantCommon    Role = "tenant_common"
)

// Valid reports whether r is one of the four ADR 0073 D3 roles. Caller
// MUST gate any role-driven decision on this — an unknown role landing in
// TimeoutsForRole returns ErrUnknownRole, which is a fail-closed signal
// the caller should surface as 401.
func (r Role) Valid() bool {
	switch r {
	case RoleMaster, RoleTenantGerente, RoleTenantAtendente, RoleTenantCommon:
		return true
	}
	return false
}

// IsMaster reports whether r is the master operator role. Callers use
// this to pick the master cookie bucket and the master rate-limit policy.
func (r Role) IsMaster() bool { return r == RoleMaster }
