package main

// SIN-63361 wiring — user-side TOTP (2FA) enforcement.
//
// internal/adapter/httpapi/usermfa shipped the handlers and adapter
// bridges in earlier ADR-0102 batches but no file under cmd/ imported
// the package. cmd/server therefore booted without the MFA-aware
// POST /login wrapper or the /admin/2fa/{setup,verify,regenerate}
// routes — the staging seed's totp_required_at=now() flag on
// admin@acme was read by nobody and the SIN-63359 Lens 1 re-sweep
// surfaced the gap (POST /login → 302 /hello-tenant, every probed
// enrollment surface 404).
//
// buildUserMFAStack assembles the four handlers (LoginPost, Setup,
// Verify, Regenerate) on top of the shared IAM pgxpool. Adapter
// constructors that need a tenant id resolve it from request context
// (tenancy.FromContext) instead of being constructed once at boot,
// because the tenant-scoped postgres adapters take tenantID at
// construction. cmd/server stays single-pool by wrapping each port
// behind a closure that does the resolve + adapter build per call;
// the cost of one tenant.ID lookup per request is dominated by the
// SQL round-trip the adapter then makes.
//
// Returns a stack with nil routes and a no-op cleanup whenever a
// required input is missing (pool, seed key, audit logger). cmd/server
// then boots without the MFA routes rather than panicking — the
// chi router skips each route when its slot is nil. This mirrors
// buildLGPDStack so a fault in one boot dependency does not take down
// the rest of the auth surface.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/crypto/aesgcm"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa"
	usermfaadapter "github.com/pericles-luz/crm/internal/adapter/usermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/tenancy"
)

const (
	// envUserMFASeedKey is the base64-encoded 32-byte AES-256-GCM key
	// the SeedCipher uses to encrypt TOTP seeds before they are
	// persisted in user_mfa.totp_seed_encrypted. Operators rotate it
	// the same way they rotate any other secret env. Unset → the MFA
	// stack stays disabled (seeds cannot be persisted without a key)
	// and cmd/server keeps booting; the chi router omits every
	// /admin/2fa route and POST /login falls back to the password-only
	// handler.
	envUserMFASeedKey = "USERMFA_SEED_KEY"

	// envUserMFAIssuer overrides the otpauth:// issuer string the
	// Authenticator app displays above the user's email. Unset falls
	// back to defaultUserMFAIssuer.
	envUserMFAIssuer = "USERMFA_ISSUER"

	// defaultUserMFAIssuer is the brand string we ship to authenticator
	// apps when the operator has not overridden it. Matches the brand
	// already in user-facing copy across the tenant surface.
	defaultUserMFAIssuer = "Sindireceita"

	// envUserMFASessionTTL tunes the post-verify tenant session
	// lifetime. Falls back to usermfa.DefaultSessionTTL (8h) when
	// unset or unparseable.
	envUserMFASessionTTL = "USERMFA_SESSION_TTL"

	// envUserMFAPendingTTL tunes the pre-verify pending cookie
	// lifetime. Falls back to usermfa.DefaultPendingTTL (5m) when
	// unset or unparseable.
	envUserMFAPendingTTL = "USERMFA_PENDING_TTL"
)

// userMFAStack bundles the routes payload the router consumes plus a
// cleanup hook for completeness. Cleanup is non-nil even when Routes
// is empty so the caller can defer it without a nil-check (mirrors
// lgpdStack).
type userMFAStack struct {
	Routes  httpapi.UserMFARoutes
	Cleanup func()
}

// noopUserMFAStack returns a stack with no mounted routes and a no-op
// cleanup.
func noopUserMFAStack() userMFAStack {
	return userMFAStack{Cleanup: func() {}}
}

// buildUserMFAStack assembles the SIN-63361 MFA-aware handlers.
//
// pool is the IAM runtime pgxpool (reused so we don't open a second
// connection set). iamSvc is the per-request IAM authenticator the
// LoginPost handler delegates the password check to. splitLogger is
// the audit_log_security writer (shared with the LGPD wire). getenv
// sources the seed key + issuer + TTL knobs.
//
// Returns noopUserMFAStack() on any missing input — the router then
// omits every /admin/2fa route AND falls back to the password-only
// handler.LoginPost. Failure paths are non-fatal: a misconfigured
// MFA secret should not block the rest of the auth surface from
// serving.
func buildUserMFAStack(_ context.Context, pool *pgxpool.Pool, iamSvc usermfa.LoginAuthenticator, splitLogger audit.SplitLogger, getenv func(string) string) userMFAStack {
	if pool == nil || iamSvc == nil || splitLogger == nil {
		return noopUserMFAStack()
	}

	cipher, err := buildUserMFASeedCipher(getenv)
	if err != nil {
		log.Printf("crm: usermfa disabled — seed cipher: %v", err)
		return noopUserMFAStack()
	}

	issuer := strings.TrimSpace(getenv(envUserMFAIssuer))
	if issuer == "" {
		issuer = defaultUserMFAIssuer
	}

	hasher := aesgcm.NewRecoveryHasher()
	alerter := usermfaadapter.NoopAlerter{}
	logger := slog.Default()
	sessions := postgresadapter.NewSessionStore(pool)

	labels, err := postgresadapter.NewTenantUserLabel(pool)
	if err != nil {
		log.Printf("crm: usermfa disabled — label reader: %v", err)
		return noopUserMFAStack()
	}

	pendings := &tenantPendingsBridge{pool: pool}
	requirements := &tenantRequirementsBridge{pool: pool}
	enrollment := &tenantEnrollmentBridge{pool: pool}
	failures := usermfa.NewMemoryFailureCounter(0)
	auditBridge := &tenantUserMFAAuditBridge{writer: splitLogger}

	mfaSvc := &tenantMFAServiceBridge{
		pool:        pool,
		cipher:      cipher,
		hasher:      hasher,
		alerter:     alerter,
		splitLogger: splitLogger,
		issuer:      issuer,
	}

	sessionMinter, err := usermfa.NewTenantSessionMinter(sessions, readUserMFASessionTTL(getenv))
	if err != nil {
		log.Printf("crm: usermfa disabled — session minter: %v", err)
		return noopUserMFAStack()
	}

	handler, err := usermfa.NewHandler(usermfa.HandlerConfig{
		Enroller:      mfaSvc,
		Verifier:      mfaSvc,
		Consumer:      mfaSvc,
		Regenerator:   mfaSvc,
		Pendings:      pendings,
		Enrollment:    enrollment,
		SessionMinter: sessionMinter,
		Failures:      failures,
		Audit:         auditBridge,
		Labels:        labels,
		Logger:        logger,
	})
	if err != nil {
		log.Printf("crm: usermfa disabled — handler: %v", err)
		return noopUserMFAStack()
	}

	loginPost := usermfa.LoginPost(usermfa.LoginConfig{
		IAM:          iamSvc,
		Sessions:     sessions,
		Pendings:     pendings,
		Requirements: requirements,
		PendingTTL:   readUserMFAPendingTTL(getenv),
		Logger:       logger,
	})

	routes := httpapi.UserMFARoutes{
		LoginPost:  loginPost,
		Setup:      http.HandlerFunc(handler.Setup),
		Verify:     http.HandlerFunc(handler.Verify),
		Regenerate: http.HandlerFunc(handler.Regenerate),
	}

	log.Printf("crm: usermfa /admin/2fa/{setup,verify,regenerate} + MFA-aware POST /login mounted (issuer=%q)", issuer)
	return userMFAStack{
		Routes:  routes,
		Cleanup: func() {},
	}
}

// buildUserMFASeedCipher decodes envUserMFASeedKey, validates it is
// exactly 32 bytes, and wraps it in aesgcm.SeedCipher. Returns a
// descriptive error so the disable log explains why MFA stays off
// (missing env vs malformed base64 vs wrong key length).
func buildUserMFASeedCipher(getenv func(string) string) (mfa.SeedCipher, error) {
	raw := strings.TrimSpace(getenv(envUserMFASeedKey))
	if raw == "" {
		return nil, fmt.Errorf("%s unset", envUserMFASeedKey)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s base64 decode: %w", envUserMFASeedKey, err)
	}
	if len(key) != aesgcm.KeySize {
		return nil, fmt.Errorf("%s must decode to %d bytes (got %d)", envUserMFASeedKey, aesgcm.KeySize, len(key))
	}
	return aesgcm.New(key, rand.Reader)
}

// readUserMFASessionTTL parses envUserMFASessionTTL. A zero or
// unparseable value falls back to usermfa.DefaultSessionTTL.
func readUserMFASessionTTL(getenv func(string) string) time.Duration {
	raw := strings.TrimSpace(getenv(envUserMFASessionTTL))
	if raw == "" {
		return usermfa.DefaultSessionTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return usermfa.DefaultSessionTTL
	}
	return d
}

// readUserMFAPendingTTL parses envUserMFAPendingTTL. A zero or
// unparseable value falls back to usermfa.DefaultPendingTTL.
func readUserMFAPendingTTL(getenv func(string) string) time.Duration {
	raw := strings.TrimSpace(getenv(envUserMFAPendingTTL))
	if raw == "" {
		return usermfa.DefaultPendingTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return usermfa.DefaultPendingTTL
	}
	return d
}

// ---- Per-request tenant bridges ----
//
// The postgres-side MFA adapters take tenantID at construction time
// (RLS isolation is per-connection). cmd/server boots once and does
// not know which tenant will serve which request, so these bridges
// resolve tenant from request context on every call and construct the
// matching adapter on the fly. The hot path is one tenancy.FromContext
// + one constructor call before each SQL round-trip; the constructor
// is allocation-light (records pool pointer + tenantID).

// tenantPendingsBridge satisfies usermfa.PendingStore +
// usermfa.PendingCreator on top of TenantUserMFAPending.
type tenantPendingsBridge struct {
	pool *pgxpool.Pool
}

func (b *tenantPendingsBridge) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (usermfa.Pending, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return usermfa.Pending{}, err
	}
	adapter, err := postgresadapter.NewTenantUserMFAPending(b.pool, tenantID)
	if err != nil {
		return usermfa.Pending{}, fmt.Errorf("usermfa wire: pendings create: %w", err)
	}
	return usermfa.NewPendingsBridge(adapter).Create(ctx, userID, ttl, nextPath)
}

func (b *tenantPendingsBridge) Get(ctx context.Context, id uuid.UUID) (usermfa.Pending, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return usermfa.Pending{}, err
	}
	adapter, err := postgresadapter.NewTenantUserMFAPending(b.pool, tenantID)
	if err != nil {
		return usermfa.Pending{}, fmt.Errorf("usermfa wire: pendings get: %w", err)
	}
	return usermfa.NewPendingsBridge(adapter).Get(ctx, id)
}

func (b *tenantPendingsBridge) Delete(ctx context.Context, id uuid.UUID) error {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return err
	}
	adapter, err := postgresadapter.NewTenantUserMFAPending(b.pool, tenantID)
	if err != nil {
		return fmt.Errorf("usermfa wire: pendings delete: %w", err)
	}
	return usermfa.NewPendingsBridge(adapter).Delete(ctx, id)
}

// tenantRequirementsBridge satisfies usermfa.RequirementReader.
type tenantRequirementsBridge struct {
	pool *pgxpool.Pool
}

func (b *tenantRequirementsBridge) Load(ctx context.Context, userID uuid.UUID) (usermfa.Requirement, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return usermfa.Requirement{}, err
	}
	adapter, err := postgresadapter.NewTenantUserMFARequirement(b.pool, tenantID)
	if err != nil {
		return usermfa.Requirement{}, fmt.Errorf("usermfa wire: requirements: %w", err)
	}
	return usermfa.NewRequirementsBridge(adapter).Load(ctx, userID)
}

// tenantEnrollmentBridge satisfies usermfa.EnrollmentChecker.
type tenantEnrollmentBridge struct {
	pool *pgxpool.Pool
}

func (b *tenantEnrollmentBridge) IsEnrolled(ctx context.Context, userID uuid.UUID) (bool, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return false, err
	}
	adapter, err := postgresadapter.NewTenantUserMFA(b.pool, tenantID)
	if err != nil {
		return false, fmt.Errorf("usermfa wire: enrollment: %w", err)
	}
	return adapter.IsEnrolled(ctx, userID)
}

// tenantUserMFAAuditBridge satisfies usermfa.AuditEmitter by routing
// every event into the shared SplitLogger after stamping the row with
// the request-context tenant id.
type tenantUserMFAAuditBridge struct {
	writer audit.SplitLogger
}

func (b *tenantUserMFAAuditBridge) LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		// Audit emit failures must not block a deny; log via the audit
		// path with a zero tenant so the row is still created and the
		// dashboard sees the bypass attempt. The handler logs the
		// underlying err via its own logger.
		tenantID = uuid.Nil
	}
	logger, err := usermfaadapter.NewTenantAuditLogger(b.writer, tenantOrSentinel(tenantID))
	if err != nil {
		return fmt.Errorf("usermfa wire: audit: %w", err)
	}
	return logger.LogMFARequired(ctx, userID, route, reason)
}

// tenantOrSentinel guards NewTenantAuditLogger's nil-tenant validation.
// The audit row carries the resolved tenant when present; when the
// request context lost it (a defensive path that should not happen in
// production under TenantScope middleware) we route through a sentinel
// so the bypass attempt still makes it into the security ledger.
func tenantOrSentinel(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil {
		return uuid.MustParse("00000000-0000-0000-0000-000000000001")
	}
	return id
}

// tenantMFAServiceBridge satisfies usermfa.Enroller, Verifier,
// RecoveryConsumer, and RecoveryRegenerator by constructing a
// per-request mfa.Service whose SeedRepository / RecoveryStore /
// AuditLogger are tenant-scoped from the request context. SeedCipher,
// CodeHasher, Alerter, and Issuer are tenant-agnostic and captured
// once at construction.
type tenantMFAServiceBridge struct {
	pool        *pgxpool.Pool
	cipher      mfa.SeedCipher
	hasher      mfa.CodeHasher
	alerter     mfa.Alerter
	splitLogger audit.SplitLogger
	issuer      string
}

func (b *tenantMFAServiceBridge) Enroll(ctx context.Context, userID uuid.UUID, label string) (mfa.EnrollResult, error) {
	svc, err := b.build(ctx)
	if err != nil {
		return mfa.EnrollResult{}, err
	}
	return svc.Enroll(ctx, userID, label)
}

func (b *tenantMFAServiceBridge) Verify(ctx context.Context, userID uuid.UUID, code string) error {
	svc, err := b.build(ctx)
	if err != nil {
		return err
	}
	return svc.Verify(ctx, userID, code)
}

func (b *tenantMFAServiceBridge) ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string, reqCtx mfa.RequestContext) error {
	svc, err := b.build(ctx)
	if err != nil {
		return err
	}
	return svc.ConsumeRecovery(ctx, userID, submitted, reqCtx)
}

func (b *tenantMFAServiceBridge) RegenerateRecovery(ctx context.Context, userID uuid.UUID, reqCtx mfa.RequestContext) ([]string, error) {
	svc, err := b.build(ctx)
	if err != nil {
		return nil, err
	}
	return svc.RegenerateRecovery(ctx, userID, reqCtx)
}

func (b *tenantMFAServiceBridge) build(ctx context.Context) (*mfa.Service, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	seeds, err := postgresadapter.NewTenantUserMFA(b.pool, tenantID)
	if err != nil {
		return nil, fmt.Errorf("usermfa wire: seed repository: %w", err)
	}
	codes, err := postgresadapter.NewTenantUserRecoveryCodes(b.pool, tenantID)
	if err != nil {
		return nil, fmt.Errorf("usermfa wire: recovery store: %w", err)
	}
	auditLogger, err := usermfaadapter.NewTenantAuditLogger(b.splitLogger, tenantID)
	if err != nil {
		return nil, fmt.Errorf("usermfa wire: audit: %w", err)
	}
	return mfa.NewService(mfa.Config{
		SeedRepository: seeds,
		SeedCipher:     b.cipher,
		RecoveryStore:  codes,
		CodeHasher:     b.hasher,
		Audit:          auditLogger,
		Alerter:        b.alerter,
		Issuer:         b.issuer,
	})
}

// tenantIDFromContext returns the resolved tenant id from request
// context (placed there by middleware.TenantScope). An empty/missing
// tenant is a wireup-level error because every MFA route is mounted
// inside the tenanted chi group — reaching one of these bridges
// without a tenant on context means the middleware chain regressed.
func tenantIDFromContext(ctx context.Context) (uuid.UUID, error) {
	t, err := tenancy.FromContext(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("usermfa wire: tenant from context: %w", err)
	}
	if t == nil || t.ID == uuid.Nil {
		return uuid.Nil, errors.New("usermfa wire: tenant from context: empty tenant")
	}
	return t.ID, nil
}

// Compile-time assertions so a future port-signature change surfaces
// here instead of at runtime when the wire-built handler is first
// invoked.
var (
	_ usermfa.PendingCreator      = (*tenantPendingsBridge)(nil)
	_ usermfa.PendingStore        = (*tenantPendingsBridge)(nil)
	_ usermfa.RequirementReader   = (*tenantRequirementsBridge)(nil)
	_ usermfa.EnrollmentChecker   = (*tenantEnrollmentBridge)(nil)
	_ usermfa.Enroller            = (*tenantMFAServiceBridge)(nil)
	_ usermfa.Verifier            = (*tenantMFAServiceBridge)(nil)
	_ usermfa.RecoveryConsumer    = (*tenantMFAServiceBridge)(nil)
	_ usermfa.RecoveryRegenerator = (*tenantMFAServiceBridge)(nil)
	_ usermfa.AuditEmitter        = (*tenantUserMFAAuditBridge)(nil)
	_ iam.SessionStore            = (*postgresadapter.SessionStore)(nil)
)
