package iam

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/google/uuid"
)

// MasterCredentialReader reads the GLOBAL master operator's credentials
// keyed by email. The master operator is tenant-less: the seeded row is
// scoped to is_master = true AND tenant_id IS NULL (migrations/seed/stg.sql
// — "master surface is not bound to a tenant FQDN"). This is the master
// analogue of UserCredentialReader.LookupCredentials, minus the tenant
// scope: there is no host → tenant resolution because the master console
// is global.
//
// LookupMasterCredentials returns the user id and the encoded password
// hash for the master row with the given email. If no master row matches,
// the implementation MUST return (uuid.Nil, "", nil) — a nil error with
// zero values, NOT an error — so MasterLogin distinguishes "not found"
// from "DB error" by the zero-id sentinel and can run the timing-equivalent
// dummy verify on the miss path (anti-enumeration, same contract as the
// tenant LookupCredentials).
type MasterCredentialReader interface {
	LookupMasterCredentials(ctx context.Context, email string) (uuid.UUID, string, error)
}

// MasterLogin authenticates the GLOBAL master operator against
// (email, password) and, on success, returns a Session carrying ONLY the
// resolved UserID.
//
// It differs from Login in three deliberate ways, all rooted in the master
// operator being tenant-less (ADR 0074 master MFA + the stg.sql seed):
//
//  1. No tenant resolution. host is accepted to satisfy the shared
//     mastermfa.MasterLoginFunc signature but is intentionally ignored —
//     the master surface is not bound to a tenant FQDN, so a
//     ResolveByHost(host) → LookupCredentials(tenantID, email) chain (the
//     tenant Login path) can NEVER find a tenant_id=NULL master row. That
//     mismatch is exactly the SIN-65254 bug: /m/login was wired to the
//     tenant-scoped iam.Service.Login and returned 401 for the only seeded
//     master operator.
//  2. No tenant session is persisted. The master_session row is minted by
//     the /m/login HTTP handler (mastermfa.LoginHandler) from the UserID on
//     the returned Session; this method never touches SessionStore.
//  3. Credentials are resolved by (email, is_master=true, tenant_id IS NULL)
//     via MasterCredentialReader — a dedicated master entry point.
//
// SIN-63340 boundary: resolving the master operator through THIS dedicated
// is_master path is the legitimate master login and does NOT reopen the
// SIN-63340 elevation vector. That vector is about TENANT hosts honouring a
// users.role='master' value; the tenant Login path still excludes RoleMaster
// from resolveSessionRole's allowlist. A global master operator authenticated
// on the master surface is the intended design, not a tenant escalation.
//
// All credential-mismatch outcomes collapse to ErrInvalidCredentials (unknown
// email, wrong password) with a timing-equivalent dummy verify on the miss
// path. An active account_lockout row short-circuits to *AccountLockedError
// before any password verification (the SIN-62341 "lockout vence o counter"
// contract). Internal errors (DB outages) propagate so the HTTP layer returns
// 5xx rather than a misleading 401.
//
// ipAddr / userAgent / route ride into the master-lockout Slack alert
// (ADR 0074 §6) when the failure counter trips the m_login lockout.
func (s *Service) MasterLogin(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (Session, error) {
	logger := s.logger()

	if s.MasterUsers == nil {
		// Misconfiguration, not a credential outcome: surface it so the
		// boundary returns 5xx instead of silently rejecting every operator.
		return Session{}, fmt.Errorf("iam: master login: no master credential reader configured")
	}

	userID, encoded, err := s.MasterUsers.LookupMasterCredentials(ctx, email)
	if err != nil {
		return Session{}, fmt.Errorf("iam: lookup master credentials: %w", err)
	}

	// Anti-enumeration: unknown master email runs the same dummy argon2id
	// derivation as the verified path so the wall-clock cost matches.
	if userID == uuid.Nil {
		s.dummyVerify(password)
		logger.WarnContext(ctx, "master login: rejected", slog.String("reason", "invalid_credentials"))
		return Session{}, ErrInvalidCredentials
	}

	// Durable lockout pre-check BEFORE VerifyPassword (SIN-62341): once the
	// account_lockout row exists no password verification happens, so a
	// Redis flush cannot reset the penalty. The dummy verify keeps the
	// locked branch timing-indistinguishable from a wrong-password reject.
	if s.Lockouts != nil {
		locked, until, err := s.Lockouts.IsLocked(ctx, userID)
		if err != nil {
			return Session{}, fmt.Errorf("iam: master lockout pre-check: %w", err)
		}
		if locked {
			s.dummyVerify(password)
			logger.WarnContext(ctx, "master login: rejected",
				slog.String("reason", "account_locked"),
				slog.Time("locked_until", until),
			)
			return Session{}, &AccountLockedError{Until: until}
		}
	}

	ok, _, err := s.verifyPassword(encoded, password)
	if err != nil || !ok {
		// Record the failure; the m_login policy trips the durable lockout
		// at the threshold and fires the synchronous Slack alert (ADR 0074
		// §6). tenantID is uuid.Nil — the master operator is tenant-less.
		if until, locked := s.recordLoginFailure(ctx, uuid.Nil, userID, email, ipAddr, userAgent, route); locked {
			return Session{}, &AccountLockedError{Until: until}
		}
		logger.WarnContext(ctx, "master login: rejected", slog.String("reason", "invalid_credentials"))
		return Session{}, ErrInvalidCredentials
	}

	// Successful verify: best-effort, idempotent lockout reset. A Clear
	// failure does not abort the login — the operator has authenticated.
	if s.Lockouts != nil {
		if err := s.Lockouts.Clear(ctx, userID); err != nil {
			logger.WarnContext(ctx, "master login: clear lockout failed",
				slog.String("user_id", userID.String()),
				slog.String("err", err.Error()),
			)
		}
	}

	logger.InfoContext(ctx, "master login: ok", slog.String("user_id", userID.String()))
	// Tenant-less session: only UserID is populated. The /m/login handler
	// reads it to mint the __Host-sess-master master_session row.
	return Session{UserID: userID}, nil
}
