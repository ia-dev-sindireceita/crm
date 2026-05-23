package main

// SIN-63337 / SIN-63338 — tenant 2FA wireup (Fase 6 F11).
//
// Assembles the user-side 2FA handler chain (/admin/2fa/setup, /verify,
// /regenerate) AND the MFA-aware POST /login wrapper so the tenant
// hexagonal-port + adapter graph from SIN-63184 reaches production
// traffic. F11 closed the disclosure that the handler shipped but was
// never mounted; this wire is the closure.
//
// The wire is fail-soft: when DATABASE_URL or IAM_MFA_SEED_KEY is unset
// the handler + login wrapper are nil and the router skips the routes
// (clean 404) AND keeps the password-only POST /login intact. Staging
// MUST provision IAM_MFA_SEED_KEY (32-byte hex-encoded AES-256 key) so
// the routes mount with the full handler — see PR description for the
// secret-provisioning checklist.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/crypto/aesgcm"
	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa"
	usermfaadapter "github.com/pericles-luz/crm/internal/adapter/usermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// envMFASeedKey is the env var name for the AES-256 key (hex-encoded,
// 64 chars) used by aesgcm.SeedCipher to wrap TOTP seeds at rest. The
// same key MUST be supplied to every replica — a key rotation requires
// a re-enrol of every TOTP-enabled user (the encrypted seeds become
// unreadable on key change). Out of scope here: rotation tooling lives
// in the operational follow-up.
const envMFASeedKey = "IAM_MFA_SEED_KEY"

// mfaIssuer is the otpauth:// URI issuer shown by authenticator apps
// above the per-user label.
const mfaIssuer = "Sindireceita"

// userMFAStack bundles the two http.Handler slots the chi router consumes:
//
//   - Handler dispatches GET/POST /admin/2fa/setup, GET/POST
//     /admin/2fa/verify, POST /admin/2fa/regenerate. Already wrapped
//     in a chi sub-router so the outer router only needs to mount it
//     under /admin/2fa/* with the per-route pending-cookie wrapper.
//   - LoginPost is the MFA-aware POST /login handler. When non-nil the
//     router swaps it in for the password-only handler.LoginPost, so
//     every authenticated user gets routed through the TOTP-requirement
//     check before a tenant session cookie is minted.
type userMFAStack struct {
	Handler   http.Handler
	LoginPost http.Handler
}

// buildUserMFAStack assembles the tenant 2FA wire. Returns a zero
// userMFAStack (both fields nil) when DATABASE_URL or IAM_MFA_SEED_KEY
// is unset or malformed — the router treats both fields as opt-in and
// keeps the pre-PR behaviour intact.
//
// The IAM dependency carries the password-only handler.LoginPost
// collaborators (IAM.Login, IAM.SessionStore for the rollback path); the
// MFA-aware wrapper layers TOTP-requirement detection on top.
func buildUserMFAStack(_ context.Context, getenv func(string) string, pool *pgxpool.Pool, iamLogin usermfa.LoginAuthenticator, sessions iam.SessionStore) userMFAStack {
	if pool == nil {
		log.Printf("crm: user MFA disabled — pool is nil")
		return userMFAStack{}
	}
	if iamLogin == nil {
		log.Printf("crm: user MFA disabled — IAM login is nil")
		return userMFAStack{}
	}
	if sessions == nil {
		log.Printf("crm: user MFA disabled — sessions is nil")
		return userMFAStack{}
	}
	rawKey := getenv(envMFASeedKey)
	if rawKey == "" {
		log.Printf("crm: user MFA disabled — %s is unset", envMFASeedKey)
		return userMFAStack{}
	}
	key, err := hex.DecodeString(rawKey)
	if err != nil {
		log.Printf("crm: user MFA disabled — %s is not valid hex: %v", envMFASeedKey, err)
		return userMFAStack{}
	}
	if len(key) != aesgcm.KeySize {
		log.Printf("crm: user MFA disabled — %s decoded to %d bytes, want %d", envMFASeedKey, len(key), aesgcm.KeySize)
		return userMFAStack{}
	}
	cipher, err := aesgcm.New(key, nil)
	if err != nil {
		log.Printf("crm: user MFA disabled — aesgcm.New: %v", err)
		return userMFAStack{}
	}

	logger := slog.Default()

	// Audit writer (audit_log_security) — shared between the mfa.Service
	// (LogEnrolled / LogVerified / LogRecoveryUsed / LogRecoveryRegenerated)
	// and the handler's bypass-attempt path (LogMFARequired). The
	// per-tenant logger is built lazily from request context.
	splitWriter, err := postgresadapter.NewSplitAuditLogger(pool)
	if err != nil {
		log.Printf("crm: user MFA disabled — split audit logger: %v", err)
		return userMFAStack{}
	}

	// Tenant-aware bridges. The postgres-side ports are constructor-
	// scoped to a tenant uuid, but the user-facing /admin/2fa routes
	// need a single chi handler that serves every tenant. Each bridge
	// resolves tenant.ID from the request context at call time and
	// instantiates the per-tenant adapter on demand.
	pendingsInner := &tenantPendingsInner{pool: pool}
	pendings := usermfa.NewPendingsBridge(pendingsInner)

	requirementsInner := &tenantRequirementsInner{pool: pool}
	requirements := usermfa.NewRequirementsBridge(requirementsInner)

	enrollment := &tenantEnrollmentBridge{pool: pool}

	labels, err := postgresadapter.NewTenantUserLabel(pool)
	if err != nil {
		log.Printf("crm: user MFA disabled — TenantUserLabel: %v", err)
		return userMFAStack{}
	}

	mfaSvcBuilder := newTenantMFAServiceBuilder(pool, cipher, splitWriter, logger)
	auditBridge := &tenantAuditBridge{writer: splitWriter}

	// Session minter for the post-verify path. Reuses the iam.SessionStore
	// to keep the post-MFA session row indistinguishable from a no-MFA
	// login.
	minter, err := usermfa.NewTenantSessionMinter(sessions, usermfa.DefaultSessionTTL)
	if err != nil {
		log.Printf("crm: user MFA disabled — session minter: %v", err)
		return userMFAStack{}
	}

	// FailureCounter: in-process map. ADR 0073 §D6: this matches the
	// single-replica staging deployment; multi-replica rollout requires
	// a Redis-backed counter (the FailureCounter port is unchanged).
	failures := usermfa.NewMemoryFailureCounter(usermfa.DefaultLockoutWindow)

	h, err := usermfa.NewHandler(usermfa.HandlerConfig{
		Enroller:      mfaSvcBuilder,
		Verifier:      mfaSvcBuilder,
		Consumer:      mfaSvcBuilder,
		Regenerator:   mfaSvcBuilder,
		Pendings:      pendings,
		Enrollment:    enrollment,
		SessionMinter: minter,
		Failures:      failures,
		Audit:         auditBridge,
		Labels:        labels,
		Logger:        logger,
	})
	if err != nil {
		log.Printf("crm: user MFA disabled — NewHandler: %v", err)
		return userMFAStack{}
	}

	// Mount the handler methods on a chi sub-router under /admin/2fa.
	// The outer router (httpapi/router.go) mounts this sub-router with
	// the pending-cookie redirect wrapper in front.
	mux := chi.NewRouter()
	mux.MethodFunc(http.MethodGet, "/admin/2fa/setup", h.Setup)
	mux.MethodFunc(http.MethodPost, "/admin/2fa/setup", h.Setup)
	mux.MethodFunc(http.MethodGet, "/admin/2fa/verify", h.Verify)
	mux.MethodFunc(http.MethodPost, "/admin/2fa/verify", h.Verify)
	mux.MethodFunc(http.MethodPost, "/admin/2fa/regenerate", h.Regenerate)

	loginPost := usermfa.LoginPost(usermfa.LoginConfig{
		IAM:          iamLogin,
		Sessions:     sessionDeleteBridge{inner: sessions},
		Pendings:     pendings,
		Requirements: requirements,
		PendingTTL:   usermfa.DefaultPendingTTL,
		Logger:       logger,
	})

	return userMFAStack{
		Handler:   mux,
		LoginPost: loginPost,
	}
}

// sessionDeleteBridge adapts iam.SessionStore.Delete (tenantID + sessionID)
// to usermfa.SessionDeleter (identical signature). The named type keeps the
// wire honest about the dependency direction — the iam package does not
// import usermfa.
type sessionDeleteBridge struct {
	inner iam.SessionStore
}

func (b sessionDeleteBridge) Delete(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return b.inner.Delete(ctx, tenantID, sessionID)
}

// ---------------------------------------------------------------------
// tenant-aware port bridges
// ---------------------------------------------------------------------

// tenantFromCtx pulls the tenancy.Tenant id from request context.
// Returns uuid.Nil + a descriptive error when the context was not
// threaded through middleware.TenantScope; callers MUST surface this
// as an internal error rather than degrading silently because the
// per-tenant adapter cannot be built without it.
func tenantFromCtx(ctx context.Context) (uuid.UUID, error) {
	t, err := tenancy.FromContext(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("usermfa wire: tenant from context: %w", err)
	}
	if t.ID == uuid.Nil {
		return uuid.Nil, errors.New("usermfa wire: tenant id is uuid.Nil")
	}
	return t.ID, nil
}

// tenantPendingsInner satisfies usermfa.PendingsInner. Each call
// resolves the tenant from request context and instantiates a
// per-tenant *TenantUserMFAPending on demand. usermfa.PendingsBridge
// wraps this with the usermfa.Pending boundary type so the handler
// never sees the postgres row type directly.
type tenantPendingsInner struct {
	pool *pgxpool.Pool
}

func (b *tenantPendingsInner) inner(ctx context.Context) (*postgresadapter.TenantUserMFAPending, error) {
	tid, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	return postgresadapter.NewTenantUserMFAPending(b.pool, tid)
}

func (b *tenantPendingsInner) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (postgresadapter.PendingMFASession, error) {
	inner, err := b.inner(ctx)
	if err != nil {
		return postgresadapter.PendingMFASession{}, err
	}
	return inner.Create(ctx, userID, ttl, nextPath)
}

func (b *tenantPendingsInner) Get(ctx context.Context, id uuid.UUID) (postgresadapter.PendingMFASession, error) {
	inner, err := b.inner(ctx)
	if err != nil {
		return postgresadapter.PendingMFASession{}, err
	}
	return inner.Get(ctx, id)
}

func (b *tenantPendingsInner) Delete(ctx context.Context, id uuid.UUID) error {
	inner, err := b.inner(ctx)
	if err != nil {
		return err
	}
	return inner.Delete(ctx, id)
}

// tenantRequirementsInner satisfies usermfa.RequirementsInner with the
// same tenant-context lazy-build pattern as tenantPendingsInner.
type tenantRequirementsInner struct {
	pool *pgxpool.Pool
}

func (b *tenantRequirementsInner) Load(ctx context.Context, userID uuid.UUID) (postgresadapter.UserMFARequirement, error) {
	tid, err := tenantFromCtx(ctx)
	if err != nil {
		return postgresadapter.UserMFARequirement{}, err
	}
	inner, err := postgresadapter.NewTenantUserMFARequirement(b.pool, tid)
	if err != nil {
		return postgresadapter.UserMFARequirement{}, err
	}
	return inner.Load(ctx, userID)
}

// tenantEnrollmentBridge satisfies usermfa.EnrollmentChecker. Builds
// the per-tenant *TenantUserMFA at call time and forwards to
// IsEnrolled.
type tenantEnrollmentBridge struct {
	pool *pgxpool.Pool
}

func (b *tenantEnrollmentBridge) IsEnrolled(ctx context.Context, userID uuid.UUID) (bool, error) {
	tid, err := tenantFromCtx(ctx)
	if err != nil {
		return false, err
	}
	inner, err := postgresadapter.NewTenantUserMFA(b.pool, tid)
	if err != nil {
		return false, err
	}
	return inner.IsEnrolled(ctx, userID)
}

// tenantAuditBridge satisfies usermfa.AuditEmitter (and mfa.AuditLogger
// for the mfa.Service path). The TenantAuditLogger is constructor-
// scoped, so the bridge resolves the tenant from request context and
// instantiates per-call. Every audit row carries actor_user_id +
// tenant_id so a master operator can correlate MFA activity with the
// responsible user.
type tenantAuditBridge struct {
	writer audit.SplitLogger
}

func (b *tenantAuditBridge) loggerFor(ctx context.Context) (*usermfaadapter.TenantAuditLogger, error) {
	tid, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	return usermfaadapter.NewTenantAuditLogger(b.writer, tid)
}

func (b *tenantAuditBridge) LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error {
	logger, err := b.loggerFor(ctx)
	if err != nil {
		return err
	}
	return logger.LogMFARequired(ctx, userID, route, reason)
}

func (b *tenantAuditBridge) LogEnrolled(ctx context.Context, userID uuid.UUID) error {
	logger, err := b.loggerFor(ctx)
	if err != nil {
		return err
	}
	return logger.LogEnrolled(ctx, userID)
}

func (b *tenantAuditBridge) LogVerified(ctx context.Context, userID uuid.UUID) error {
	logger, err := b.loggerFor(ctx)
	if err != nil {
		return err
	}
	return logger.LogVerified(ctx, userID)
}

func (b *tenantAuditBridge) LogRecoveryUsed(ctx context.Context, userID uuid.UUID) error {
	logger, err := b.loggerFor(ctx)
	if err != nil {
		return err
	}
	return logger.LogRecoveryUsed(ctx, userID)
}

func (b *tenantAuditBridge) LogRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error {
	logger, err := b.loggerFor(ctx)
	if err != nil {
		return err
	}
	return logger.LogRecoveryRegenerated(ctx, userID)
}

// tenantMFAServiceBuilder dispatches Enroll/Verify/ConsumeRecovery/
// RegenerateRecovery to a per-request mfa.Service built with the
// tenant-scoped seed + recovery adapters. The same builder satisfies
// the four interfaces the usermfa.Handler depends on (Enroller,
// Verifier, RecoveryConsumer, RecoveryRegenerator) so the handler
// holds a single collaborator value instead of four bridges.
type tenantMFAServiceBuilder struct {
	pool    *pgxpool.Pool
	cipher  mfa.SeedCipher
	hasher  mfa.CodeHasher
	writer  audit.SplitLogger
	alerter mfa.Alerter
	issuer  string
}

func newTenantMFAServiceBuilder(pool *pgxpool.Pool, cipher mfa.SeedCipher, writer audit.SplitLogger, _ *slog.Logger) *tenantMFAServiceBuilder {
	return &tenantMFAServiceBuilder{
		pool:    pool,
		cipher:  cipher,
		hasher:  recoveryCodeHasher{inner: password.Default()},
		writer:  writer,
		alerter: usermfaadapter.NoopAlerter{},
		issuer:  mfaIssuer,
	}
}

// service builds a per-request mfa.Service for the tenant resolved from
// ctx. The build is cheap (struct assembly only) so per-call allocation
// is acceptable for the low-frequency 2FA routes.
func (b *tenantMFAServiceBuilder) service(ctx context.Context) (*mfa.Service, error) {
	tid, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	seeds, err := postgresadapter.NewTenantUserMFA(b.pool, tid)
	if err != nil {
		return nil, err
	}
	codes, err := postgresadapter.NewTenantUserRecoveryCodes(b.pool, tid)
	if err != nil {
		return nil, err
	}
	audit, err := usermfaadapter.NewTenantAuditLogger(b.writer, tid)
	if err != nil {
		return nil, err
	}
	return mfa.NewService(mfa.Config{
		SeedRepository: seeds,
		SeedCipher:     b.cipher,
		RecoveryStore:  codes,
		CodeHasher:     b.hasher,
		Audit:          audit,
		Alerter:        b.alerter,
		Issuer:         b.issuer,
	})
}

func (b *tenantMFAServiceBuilder) Enroll(ctx context.Context, userID uuid.UUID, label string) (mfa.EnrollResult, error) {
	svc, err := b.service(ctx)
	if err != nil {
		return mfa.EnrollResult{}, err
	}
	return svc.Enroll(ctx, userID, label)
}

func (b *tenantMFAServiceBuilder) Verify(ctx context.Context, userID uuid.UUID, code string) error {
	svc, err := b.service(ctx)
	if err != nil {
		return err
	}
	return svc.Verify(ctx, userID, code)
}

func (b *tenantMFAServiceBuilder) ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string, reqCtx mfa.RequestContext) error {
	svc, err := b.service(ctx)
	if err != nil {
		return err
	}
	return svc.ConsumeRecovery(ctx, userID, submitted, reqCtx)
}

func (b *tenantMFAServiceBuilder) RegenerateRecovery(ctx context.Context, userID uuid.UUID, reqCtx mfa.RequestContext) ([]string, error) {
	svc, err := b.service(ctx)
	if err != nil {
		return nil, err
	}
	return svc.RegenerateRecovery(ctx, userID, reqCtx)
}

// recoveryCodeHasher adapts password.Argon2idHasher (which returns
// (ok, needsRehash, err)) to mfa.CodeHasher (which returns (ok, err)).
// Re-hash propagation does not apply to recovery codes — they are
// single-use and replaced wholesale on InvalidateAll — so the
// needsRehash bit is intentionally dropped.
type recoveryCodeHasher struct {
	inner *password.Argon2idHasher
}

func (r recoveryCodeHasher) Hash(plain string) (string, error) {
	return r.inner.Hash(plain)
}

func (r recoveryCodeHasher) Verify(stored, plain string) (bool, error) {
	ok, _, err := r.inner.Verify(stored, plain)
	return ok, err
}
