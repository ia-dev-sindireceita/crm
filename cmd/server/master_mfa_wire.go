package main

// SIN-65223 (Child B of SIN-65221) — master-side /m/* composition root.
//
// Child A (SIN-65222, master_mfa_store_wire.go) assembled the concrete
// adapters into a masterMFAStack of mastermfa ports. This file turns
// that stack into the five handlers + two middlewares httpapi.MasterDeps
// needs so internal/adapter/httpapi/router.go can mount the /m/* group
// (router.go:1557 — `if deps.Master.Login != nil`).
//
// buildMasterDeps is a pure composition step: no domain logic, every
// Config literal references a mastermfa port (or a func), never a
// concrete postgres type beyond the Child A stack. It returns the zero
// httpapi.MasterDeps{} for the noop stack so health-only / DB-less boots
// keep the /m/* surface unmounted (router sees a nil Login and skips the
// whole group — the same fail-soft contract as the rest of the wireup).
//
// AuditLogger plumbing (closes SIN-63216 AC #1): the logout handler is
// handed the SAME audit.SplitLogger the tenant POST /logout uses
// (iam_wire.go `logoutAudit`), so a master logout appends a
// SecurityEventLogout row (audience="master") to audit_log_security on
// the same ledger as tenant logout — observability before any latency
// concern.

import (
	"log/slog"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// buildMasterDeps constructs the /m/* handlers + middlewares from the
// Child A masterMFAStack. splitLogger is the shared audit_log_security
// writer (the same value wired into httpapi.Deps.AuditLogger for the
// tenant /logout); logger is the process slog.Logger.
//
// Returns the zero httpapi.MasterDeps{} when stack is the noop (Login
// nil), so cmd/server boots without the /m/* surface in health-only
// mode — the router skips a group whose Login handler is nil.
func buildMasterDeps(stack masterMFAStack, splitLogger audit.SplitLogger, logger *slog.Logger) httpapi.MasterDeps {
	// Noop stack (DB-less boot, missing master seed key / actor id, …)
	// → empty MasterDeps so router.go leaves /m/* unmounted. Guarding on
	// Login mirrors the router's own `deps.Master.Login != nil` gate.
	if stack.Login == nil {
		return httpapi.MasterDeps{}
	}

	// One master-context audit logger satisfies the MFARequiredAuditor
	// the RequireMasterMFA middleware needs (LogMFARequired → 2fa_required
	// row, nil tenant). It is the Child A masterMFAAuditLogger over the
	// shared SplitLogger — the same ledger the logout row lands on.
	mfaAuditor := newMasterMFAAuditLogger(splitLogger)

	login := mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:      stack.Login,
		Sessions:   stack.Sessions,
		HardTTL:    mastermfa.DefaultMasterHardTTL,
		Logger:     logger,
		VerifyPath: "/m/2fa/verify",
	})

	// AuditLogger == the shared SplitLogger → master logout appends a
	// SecurityEventLogout row with audience="master" (SIN-63216 AC #1).
	logout := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions:    stack.Sessions,
		AuditLogger: splitLogger,
		Logger:      logger,
		LoginPath:   "/m/login",
	})

	enroll := mastermfa.NewEnrollHandler(stack.Enroller, logger)

	// Wire ALL lockout + rotation collaborators (the production path,
	// not the degraded MarkVerified-only test path): Rotator swaps the
	// session id post-MFA; Failures/Invalidator/Alerter drive the
	// session-scoped 5-strike lockout (LockoutThreshold 0 → default 5).
	verify := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:         stack.Verifier,
		Consumer:         stack.Consumer,
		Sessions:         stack.HTTPSession,
		Rotator:          stack.HTTPSession,
		Failures:         stack.Failures,
		Invalidator:      stack.Invalidator,
		Alerter:          stack.Alerter,
		LockoutThreshold: 0, // → mastermfa.LockoutThresholdDefault (5)
		LoginPath:        "/m/login",
		Logger:           logger,
		FallbackOK:       "/m/2fa/enroll",
	})

	regenerate := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{
		Regenerator: stack.Regenerator,
		Logger:      logger,
	})

	// RequireMasterAuth gates every /m/* route except /m/login + /m/logout
	// on a valid __Host-sess-master session. Auditor is the master hard-cap
	// sink (SIN-65232): when a session crosses created_at + hard TTL the
	// storage layer reports ErrSessionHardCap and the middleware (a) clears
	// the cookie + 303s to /m/login AND (b) appends one
	// master.session.hard_cap_hit row (audience="master", nil tenant) to the
	// SAME shared SplitLogger the logout/MFA-required rows land on. The two
	// controls are independent (defense in depth): an audit-write failure
	// never blocks the session teardown. Idle bump = DefaultMasterIdleTTL.
	requireAuth := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  stack.Sessions,
		Directory: stack.Directory,
		Auditor:   newMasterSessionHardCapAuditor(splitLogger),
		Logger:    logger,
		LoginPath: "/m/login",
		IdleTTL:   mastermfa.DefaultMasterIdleTTL,
		Now:       time.Now,
	})

	// RequireMasterMFA gates the MFA-only subtree (enroll + recovery
	// regenerate) on an enrolled + verified TOTP, redirecting to
	// /m/2fa/verify otherwise.
	requireMFA := mastermfa.RequireMasterMFA(mastermfa.RequireMasterMFAConfig{
		Enrollment: stack.Enrollment,
		Sessions:   stack.HTTPSession,
		Audit:      mfaAuditor,
		Logger:     logger,
		EnrollPath: "/m/2fa/enroll",
		VerifyPath: "/m/2fa/verify",
	})

	return httpapi.MasterDeps{
		Login:             login,
		Logout:            logout,
		Enroll:            enroll,
		Verify:            verify,
		Regenerate:        regenerate,
		RequireMasterAuth: requireAuth,
		RequireMasterMFA:  requireMFA,
	}
}
