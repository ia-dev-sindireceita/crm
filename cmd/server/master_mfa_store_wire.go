package main

// SIN-65222 (Child A of SIN-65221) — master-side MFA store-stack glue.
//
// The internal/adapter/httpapi/mastermfa package shipped the master
// operator login + TOTP enrol/verify + recovery + recent-MFA handlers
// and their ports in earlier ADR-0073/0074 batches, and the four
// backing postgres adapters (master_mfa, master_recovery_code,
// master_directory, mastersession) already exist. What was missing is
// a composition-root that assembles those concrete adapters into the
// mastermfa ports so Child B (SIN-65223) can construct MasterDeps and
// mount the /m/* router group.
//
// This file is the master analogue of usermfa_wire.go. The master
// adapters differ from the tenant ones in one key way: they take
// (pool, actorID) at construction rather than resolving a tenant from
// request context, so the whole stack is built ONCE at boot — there
// are no per-request bridges here. The actorID is the master-operator
// service account resolved from MASTER_OPS_ACTOR_ID (the same env the
// master tenants surface consumes, see master_tenants_wire.go).
//
// Layering note (why an mfa.Service sits in the middle):
//
//   - postgres.MasterMFA implements mfa.SeedRepository (StoreSeed /
//     LoadSeed / MarkVerified / MarkReenrollRequired) — it is storage,
//     not the Enroller/Verifier the handlers consume. LoadSeed alone
//     also satisfies mastermfa.EnrollmentReader directly.
//   - postgres.MasterRecoveryCodes implements mfa.RecoveryStore
//     (InsertHashes / ListActive / MarkConsumed / InvalidateAll) — the
//     recovery storage, not the RecoveryConsumer/Regenerator ports.
//
// So mastermfa.{Enroller,Verifier,RecoveryConsumer,Regenerator} are
// satisfied by an *mfa.Service composed over those two repositories +
// the AES-GCM seed cipher / recovery hasher + the Slack alerter + a
// master audit logger — exactly mirroring tenantMFAServiceBridge, but
// built once instead of per request. The design-review table on the
// issue summarised the postgres adapters as satisfying the handler
// ports directly; in origin/main they are storage-level, so the
// mfa.Service composition (the tenant precedent) is the faithful glue.
//
// Returns noopMasterMFAStack() (all-nil fields) whenever a required
// input is missing — nil pool, nil login fn, nil audit writer, the
// master seed key unset/malformed, or MASTER_OPS_ACTOR_ID unset/
// invalid. cmd/server then boots without the /m/* surface rather than
// panicking, the same fail-soft contract as buildUserMFAStack and
// buildMasterTenantsStack (the router skips a route whose slot is nil).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/crypto/aesgcm"
	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa"
	slackadapter "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

const (
	// envMasterMFASeedKey is the base64-encoded 32-byte AES-256-GCM key
	// the master SeedCipher uses to encrypt TOTP seeds before they land
	// in master_mfa.totp_seed_encrypted. It is deliberately SEPARATE
	// from USERMFA_SEED_KEY (tenant seeds) so a tenant-key rotation or
	// compromise has no blast radius on master-operator MFA, and vice
	// versa. Unset → the master MFA stack stays disabled and cmd/server
	// keeps booting; the router omits the /m/2fa/* routes.
	envMasterMFASeedKey = "MASTERMFA_SEED_KEY"

	// envMasterMFAIssuer overrides the otpauth:// issuer string the
	// authenticator app shows above the master operator's email. Unset
	// falls back to defaultMasterMFAIssuer.
	envMasterMFAIssuer = "MASTERMFA_ISSUER"

	// defaultMasterMFAIssuer is the brand string master operators see in
	// their authenticator app. Distinct from the tenant issuer so an
	// operator who is both a tenant user and a master operator can tell
	// the two TOTP entries apart.
	defaultMasterMFAIssuer = "Sindireceita Master"
)

// masterMFAStack bundles the master-side MFA adapters the /m/* router
// group consumes. Every field is an interface (or func) port satisfied
// by a concrete adapter built in buildMasterMFAStack:
//
//   - Sessions / HTTPSession         → mastersession.Store + HTTPSession
//   - Enroller/Verifier/Consumer/Regenerator → one shared *mfa.Service
//   - Enrollment                     → *postgres.MasterMFA (LoadSeed)
//   - Directory                      → *postgres.MasterDirectory
//   - Failures/Invalidator/Alerter   → the verify-lockout trio
//   - Login                          → master-operator login fn (caller-supplied)
//
// HTTPSession is exposed as the concrete *mastermfa.HTTPSession because
// it satisfies four ports at once (MasterSessionMFA, MasterSessionRotator,
// MasterSessionInvalidator, MasterSessionVerifiedAtStore); Child B reads
// the same pointer into each of those router slots. Invalidator is
// surfaced separately (also the HTTPSession) so the lockout trio reads
// cleanly at the Child B call site.
type masterMFAStack struct {
	Sessions    mastermfa.SessionStore
	HTTPSession *mastermfa.HTTPSession

	Enroller    mastermfa.Enroller
	Verifier    mastermfa.Verifier
	Consumer    mastermfa.RecoveryConsumer
	Regenerator mastermfa.Regenerator
	Enrollment  mastermfa.EnrollmentReader

	Directory mastermfa.MasterUserDirectory

	Failures    mastermfa.VerifyFailureCounter
	Invalidator mastermfa.MasterSessionInvalidator
	Alerter     mastermfa.LockoutAlerter

	Login mastermfa.MasterLoginFunc
}

// noopMasterMFAStack returns an all-nil stack. cmd/server uses it as
// the fail-soft fallback so the /m/* surface stays unmounted (every
// router slot nil → clean 404) without taking down the rest of the
// server. Mirrors noopUserMFAStack / noopMasterTenantsStack.
func noopMasterMFAStack() masterMFAStack {
	return masterMFAStack{}
}

// buildMasterMFAStack assembles the master MFA adapters into the
// mastermfa ports.
//
// pool is the master-ops pgxpool (the DSN that owns master_mfa /
// master_recovery_codes / master_session / master_users); the caller
// passes the same pool the master tenants surface uses. masterLogin is
// the slice of iam.Service.Login the master login handler delegates the
// password check to. splitLogger is the shared audit_log_security
// writer. getenv sources the master seed key, issuer, Slack webhook,
// and MASTER_OPS_ACTOR_ID.
//
// Returns noopMasterMFAStack() on any missing/invalid input. Failure is
// non-fatal: a misconfigured master MFA secret must not block the rest
// of the auth surface from serving.
func buildMasterMFAStack(_ context.Context, pool *pgxpool.Pool, masterLogin mastermfa.MasterLoginFunc, splitLogger audit.SplitLogger, getenv func(string) string) masterMFAStack {
	if pool == nil || masterLogin == nil || splitLogger == nil {
		return noopMasterMFAStack()
	}

	actorRaw := strings.TrimSpace(getenv(envMasterOpsActorID))
	if actorRaw == "" {
		log.Printf("crm: master mfa disabled (%s unset)", envMasterOpsActorID)
		return noopMasterMFAStack()
	}
	actorID, err := uuid.Parse(actorRaw)
	if err != nil || actorID == uuid.Nil {
		log.Printf("crm: master mfa disabled — invalid %s: %v", envMasterOpsActorID, err)
		return noopMasterMFAStack()
	}

	cipher, err := buildMasterMFASeedCipher(getenv)
	if err != nil {
		log.Printf("crm: master mfa disabled — seed cipher: %v", err)
		return noopMasterMFAStack()
	}

	issuer := strings.TrimSpace(getenv(envMasterMFAIssuer))
	if issuer == "" {
		issuer = defaultMasterMFAIssuer
	}

	seedRepo, err := postgresadapter.NewMasterMFA(pool, actorID)
	if err != nil {
		log.Printf("crm: master mfa disabled — seed repository: %v", err)
		return noopMasterMFAStack()
	}
	recoveryStore, err := postgresadapter.NewMasterRecoveryCodes(pool, actorID)
	if err != nil {
		log.Printf("crm: master mfa disabled — recovery store: %v", err)
		return noopMasterMFAStack()
	}
	directory, err := postgresadapter.NewMasterDirectory(pool, actorID)
	if err != nil {
		log.Printf("crm: master mfa disabled — directory: %v", err)
		return noopMasterMFAStack()
	}
	sessions, err := mastersession.New(pool, actorID)
	if err != nil {
		log.Printf("crm: master mfa disabled — session store: %v", err)
		return noopMasterMFAStack()
	}
	httpSession := mastermfa.NewHTTPSession(sessions)

	// One Slack notifier drives both alert paths (recovery-used /
	// regenerated via MFAAlerter and verify-lockout via
	// VerifyLockoutAlerter). slackadapter.New tolerates an empty webhook
	// (Notify no-ops), so the wire stays unconditional and the operator
	// opts in via SLACK_WEBHOOK_URL — no second Slack client.
	notifier := slackadapter.New(getenv(envSlackWebhook))

	svc, err := mfa.NewService(mfa.Config{
		SeedRepository: seedRepo,
		SeedCipher:     cipher,
		RecoveryStore:  recoveryStore,
		CodeHasher:     aesgcm.NewRecoveryHasher(),
		Audit:          newMasterMFAAuditLogger(splitLogger),
		Alerter:        slackadapter.NewMFAAlerter(notifier),
		Issuer:         issuer,
	})
	if err != nil {
		log.Printf("crm: master mfa disabled — mfa service: %v", err)
		return noopMasterMFAStack()
	}

	log.Printf("crm: master mfa store-stack assembled (issuer=%q)", issuer)
	return masterMFAStack{
		Sessions:    sessions,
		HTTPSession: httpSession,
		Enroller:    svc,
		Verifier:    svc,
		Consumer:    svc,
		Regenerator: svc,
		Enrollment:  seedRepo,
		Directory:   directory,
		// Process-local failure counter (single master-console replica).
		// Same adapter the tenant verify path uses; the usermfa port is
		// declared structurally identical to mastermfa.VerifyFailureCounter.
		Failures:    usermfa.NewMemoryFailureCounter(0),
		Invalidator: httpSession,
		Alerter:     slackadapter.NewVerifyLockoutAlerter(notifier),
		Login:       masterLogin,
	}
}

// buildMasterMFASeedCipher decodes envMasterMFASeedKey, validates it is
// exactly 32 bytes, and wraps it in aesgcm.SeedCipher. The descriptive
// error lets the disable log distinguish missing env vs malformed
// base64 vs wrong key length. Mirrors buildUserMFASeedCipher.
func buildMasterMFASeedCipher(getenv func(string) string) (mfa.SeedCipher, error) {
	raw := strings.TrimSpace(getenv(envMasterMFASeedKey))
	if raw == "" {
		return nil, fmt.Errorf("%s unset", envMasterMFASeedKey)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s base64 decode: %w", envMasterMFASeedKey, err)
	}
	if len(key) != aesgcm.KeySize {
		return nil, fmt.Errorf("%s must decode to %d bytes (got %d)", envMasterMFASeedKey, aesgcm.KeySize, len(key))
	}
	return aesgcm.New(key, rand.Reader)
}

// masterMFAAuditLogger satisfies mfa.AuditLogger for the master side by
// routing every event into audit_log_security via audit.SplitLogger
// with TenantID = nil. Master MFA activity is not tenant-scoped — the
// SplitLogger contract explicitly allows a nil TenantID for
// master-context events — so this is the master analogue of usermfa's
// TenantAuditLogger minus the tenant stamp. The 2fa_* security events
// are reused (they are already on the SplitLogger allowlist); the nil
// tenant is what marks them as master-context in the ledger.
type masterMFAAuditLogger struct {
	writer audit.SplitLogger
}

func newMasterMFAAuditLogger(writer audit.SplitLogger) *masterMFAAuditLogger {
	return &masterMFAAuditLogger{writer: writer}
}

func (l *masterMFAAuditLogger) LogEnrolled(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FAEnroll, userID, nil)
}

func (l *masterMFAAuditLogger) LogVerified(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FAVerify, userID, nil)
}

func (l *masterMFAAuditLogger) LogRecoveryUsed(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FARecoveryUsed, userID, nil)
}

func (l *masterMFAAuditLogger) LogRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FARecoveryRegenerated, userID, nil)
}

func (l *masterMFAAuditLogger) LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error {
	target := map[string]any{}
	if route != "" {
		target["route"] = route
	}
	if reason != "" {
		target["reason"] = reason
	}
	return l.write(ctx, audit.SecurityEvent2FARequired, userID, target)
}

// write emits a master-context (nil tenant) security row.
func (l *masterMFAAuditLogger) write(ctx context.Context, evt audit.SecurityEvent, userID uuid.UUID, target map[string]any) error {
	return l.writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       evt,
		ActorUserID: userID,
		TenantID:    nil,
		Target:      target,
	})
}

// masterSessionHardCapAuditor satisfies mastermfa.MasterSessionAuditor
// for the master /m/* auth middleware (RequireMasterAuth) by routing the
// single master.session.hard_cap_hit event into audit_log_security via
// audit.SplitLogger with TenantID = nil — the same tenant-less,
// master-context contract as masterMFAAuditLogger. It is the hard-cap
// analogue of the master logout / MFA-required auditors: exactly one row
// per hard-cap breach, audience="master", appended immediately before
// the cookie clear + redirect (SIN-65232, follow-up from SIN-65223).
//
// LogHardCapHit returns the write error to the middleware, which logs it
// and proceeds — the cookie clear + 303 to /m/login are never blocked by
// an audit-write failure (SIN-62418 / ADR 0073 §D3 defense-in-depth: the
// session teardown and the audit row are independent controls).
type masterSessionHardCapAuditor struct {
	writer audit.SplitLogger
}

func newMasterSessionHardCapAuditor(writer audit.SplitLogger) *masterSessionHardCapAuditor {
	return &masterSessionHardCapAuditor{writer: writer}
}

// LogHardCapHit appends the master.session.hard_cap_hit row. now is the
// breach-detection clock the middleware threads in (the persisted row
// also carries its own DB timestamp); createdAt is the session birth so
// an operator can read the elapsed lifetime straight off the ledger.
func (l *masterSessionHardCapAuditor) LogHardCapHit(ctx context.Context, userID, sessionID uuid.UUID, createdAt, now time.Time, route string) error {
	target := map[string]any{
		"session_id": sessionID.String(),
		"audience":   "master",
	}
	if route != "" {
		target["route"] = route
	}
	if !createdAt.IsZero() {
		target["created_at"] = createdAt.UTC().Format(time.RFC3339)
	}
	if !now.IsZero() {
		target["detected_at"] = now.UTC().Format(time.RFC3339)
	}
	return l.writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       audit.SecurityEventMasterSessionHardCapHit,
		ActorUserID: userID,
		TenantID:    nil,
		Target:      target,
	})
}

// Compile-time assertions: each concrete adapter satisfies the mastermfa
// (or mfa) port it is wired into. A future port-signature change surfaces
// here at build time instead of at runtime on the first /m/* request.
var (
	_ mastermfa.SessionStore             = (*mastersession.Store)(nil)
	_ mastermfa.MasterSessionMFA         = (*mastermfa.HTTPSession)(nil)
	_ mastermfa.MasterSessionRotator     = (*mastermfa.HTTPSession)(nil)
	_ mastermfa.MasterSessionInvalidator = (*mastermfa.HTTPSession)(nil)
	_ mastermfa.EnrollmentReader         = (*postgresadapter.MasterMFA)(nil)
	_ mastermfa.MasterUserDirectory      = (*postgresadapter.MasterDirectory)(nil)
	_ mastermfa.Enroller                 = (*mfa.Service)(nil)
	_ mastermfa.Verifier                 = (*mfa.Service)(nil)
	_ mastermfa.RecoveryConsumer         = (*mfa.Service)(nil)
	_ mastermfa.Regenerator              = (*mfa.Service)(nil)
	_ mastermfa.VerifyFailureCounter     = (*usermfa.MemoryFailureCounter)(nil)
	_ mastermfa.LockoutAlerter           = (*slackadapter.VerifyLockoutAlerter)(nil)

	// mfa.Service collaborators built in this file.
	_ mfa.SeedRepository = (*postgresadapter.MasterMFA)(nil)
	_ mfa.RecoveryStore  = (*postgresadapter.MasterRecoveryCodes)(nil)
	_ mfa.AuditLogger    = (*masterMFAAuditLogger)(nil)
	_ mfa.Alerter        = (*slackadapter.MFAAlerter)(nil)

	// SIN-65232: the hard-cap auditor satisfies the master-auth port.
	_ mastermfa.MasterSessionAuditor = (*masterSessionHardCapAuditor)(nil)
)
