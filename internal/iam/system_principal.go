package iam

import (
	"errors"

	"github.com/google/uuid"
)

// SIN-66305 (R3 / SIN-66292) — reserved SYSTEM principal.
//
// The WhatsApp-session ban/disconnect audit (SIN-66260 Fase 5) records a
// tamper-evident row in audit_log_security on each terminal transition.
// That ledger requires a non-nil actor_user_id (the SplitLogger fail-closed
// guard rejects uuid.Nil) FK-referencing users(id), but a ban is an
// actor-less, asynchronous system event — no operator is in the request
// path. We attribute it to ONE reserved, non-human principal seeded by
// migration 0126: a master row (is_master = true, tenant_id IS NULL) flagged
// is_system = true.
//
// The principal is hardened so it can never become an authentication or
// privilege-amplification vector (the CTO/SecEng merge gates on SIN-66305):
//
//   - Gate 1 — it carries an un-decodable password sentinel
//     (PasswordSentinelHash), not a real argon2id hash. password.Verify
//     errors decoding it, so MasterLogin always collapses to
//     ErrInvalidCredentials. Login fails closed.
//   - Gate 2 — every master-ops read/mutate surface excludes is_system
//     centrally: the master credential reader (the single login-resolution
//     choke point) and the master directory filter `AND is_system = false`;
//     SetPassword refuses SystemPrincipalID via IsSystemPrincipal. Because
//     credential resolution excludes it, no post-login master surface (MFA
//     enroll, recovery codes, lockout, session) is ever reachable for it.
//   - Gate 3 — fixed reserved UUID + reserved, non-deliverable email
//     (.invalid TLD, RFC 2606).
//   - Gate 4 — no session is ever minted: login fails closed (gate 1) and
//     tenant_id IS NULL means impersonation (keyed by target_tenant_id) can
//     never target it.
const (
	// SystemPrincipalEmail is the reserved, non-deliverable address of the
	// system principal. The .invalid TLD (RFC 2606) guarantees it can never
	// receive mail, so password-reset / re-invite flows have nothing to
	// deliver even if one were ever wired to reach it.
	SystemPrincipalEmail = "system+wa-session@host.invalid"

	// PasswordSentinelHash is the literal stored in the system principal's
	// password_hash column. It is intentionally NOT a PHC/argon2id string:
	// password.Verify's decode step rejects it (ErrInvalidEncoding), so any
	// authentication attempt fails closed. It is NOT a hash of any secret —
	// there is no recoverable password.
	PasswordSentinelHash = "SYSTEM-NO-LOGIN"
)

// SystemPrincipalID is the reserved fixed UUID of the system principal,
// pinned via uuid.MustParse so it survives package init without a global
// mutable. It MUST match the id seeded by migration 0126.
var systemPrincipalID = uuid.MustParse("00000000-0000-0000-0000-000000005a5e")

// SystemPrincipalID returns the reserved system-principal UUID. Exposed as
// a function (not an exported var) so accidental package-level mutation is
// impossible — the same hardening the funnel SystemActor uses.
func SystemPrincipalID() uuid.UUID { return systemPrincipalID }

// IsSystemPrincipal reports whether id is the reserved system principal.
// Mutation choke points (SetPassword, and any future master-user write
// path) call this to refuse operating on it — a central, DB-read-free
// default-exclusion keyed off the reserved fixed UUID (gate 2).
func IsSystemPrincipal(id uuid.UUID) bool { return id == systemPrincipalID }

// ErrSystemPrincipalProtected is returned by mutation paths asked to act on
// the reserved system principal. It is a typed sentinel so callers can
// errors.Is it and render a deliberate refusal rather than a generic 5xx.
var ErrSystemPrincipalProtected = errors.New("iam: operation refused on reserved system principal")
