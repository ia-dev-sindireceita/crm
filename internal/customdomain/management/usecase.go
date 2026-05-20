package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// defaultTokenTTL is the verification-token expiry window used when
// Config.TokenTTL is zero. SIN-63104.
const defaultTokenTTL = 24 * time.Hour

// UseCase is the management orchestrator. Construct once with New and
// reuse; safe for concurrent calls as long as every embedded port is.
type UseCase struct {
	store     Store
	gate      EnrollmentGate
	validator HostValidator
	dns       DNSChecker
	slug      SlugReleaser
	audit     AuditLogger
	tokenGen  TokenGenerator
	now       Clock
	tokenTTL  time.Duration
}

// Config groups dependencies so callers can leave the optional ones zero
// without juggling positional nils.
type Config struct {
	Store     Store
	Gate      EnrollmentGate
	Validator HostValidator // optional — nil disables host pre-validation
	DNS       DNSChecker    // optional — nil makes Verify return ReasonInternal
	Slug      SlugReleaser  // optional — nil skips slug-reservation on delete (test-only)
	Audit     AuditLogger
	TokenGen  TokenGenerator // optional — defaults to crypto/rand
	Now       Clock          // optional — defaults to time.Now
	// TokenTTL bounds how long a verification token stays acceptable for
	// Verify after enrollment. Defaults to 24h when zero. SIN-63104.
	TokenTTL time.Duration
}

// New wires the use-case from a Config. Returns an error when the
// non-optional ports are missing.
func New(cfg Config) (*UseCase, error) {
	if cfg.Store == nil {
		return nil, errors.New("management: Store is required")
	}
	if cfg.Gate == nil {
		return nil, errors.New("management: Gate is required")
	}
	tokenGen := cfg.TokenGen
	if tokenGen == nil {
		tokenGen = defaultTokenGen
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	return &UseCase{
		store:     cfg.Store,
		gate:      cfg.Gate,
		validator: cfg.Validator,
		dns:       cfg.DNS,
		slug:      cfg.Slug,
		audit:     cfg.Audit,
		tokenGen:  tokenGen,
		now:       now,
		tokenTTL:  ttl,
	}, nil
}

// List returns the active (non-soft-deleted) domains for tenantID,
// newest first. Errors propagate as-is; the boundary maps to 5xx.
func (u *UseCase) List(ctx context.Context, tenantID uuid.UUID) ([]Domain, error) {
	if tenantID == uuid.Nil {
		return nil, ErrTenantMismatch
	}
	return u.store.List(ctx, tenantID)
}

// Get returns a single domain by id, scoped to tenantID. Returns
// ErrTenantMismatch if the row belongs to another tenant — never leak
// rows across tenants.
func (u *UseCase) Get(ctx context.Context, tenantID, id uuid.UUID) (Domain, error) {
	if tenantID == uuid.Nil {
		return Domain{}, ErrTenantMismatch
	}
	d, err := u.store.GetByID(ctx, id)
	if err != nil {
		return Domain{}, err
	}
	if d.TenantID != tenantID {
		return Domain{}, ErrTenantMismatch
	}
	return d, nil
}

// Enroll runs the full deny-by-default pipeline for a new claim:
//
//  1. Normalize + validate the host.
//  2. Check the per-tenant quota gate (rate limit / hard cap / breaker).
//  3. Generate the verification token.
//  4. Insert the row in tenant_custom_domains.
//
// The wizard step 2 reads EnrollResult.TXTRecord/TXTValue to render the
// instructions. ReasonRateLimited carries RetryAfter so the boundary
// can craft the PT-BR string.
func (u *UseCase) Enroll(ctx context.Context, tenantID uuid.UUID, rawHost string) (EnrollResult, error) {
	if tenantID == uuid.Nil {
		return EnrollResult{Reason: ReasonForbidden}, ErrTenantMismatch
	}
	host, err := NormalizeHost(rawHost)
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: rawHost, Action: "enroll", Outcome: "denied:invalid_host", Reason: ReasonInvalidHost, At: u.now()})
		return EnrollResult{Reason: ReasonInvalidHost}, ErrInvalidHost
	}
	if u.validator != nil {
		if err := u.validator.Validate(ctx, host); err != nil {
			reason := classifyValidationError(err)
			u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: host, Action: "enroll", Outcome: "denied:" + reason.String(), Reason: reason, At: u.now()})
			return EnrollResult{Reason: reason}, err
		}
	}

	dec := u.gate.Allow(ctx, tenantID)
	if dec.Err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: host, Action: "enroll", Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return EnrollResult{Reason: ReasonInternal}, dec.Err
	}
	if !dec.Allowed {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: host, Action: "enroll", Outcome: "denied:" + dec.Reason.String(), Reason: dec.Reason, At: u.now()})
		return EnrollResult{Reason: dec.Reason, RetryAfter: dec.RetryAfter}, nil
	}

	token, err := u.tokenGen()
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: host, Action: "enroll", Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return EnrollResult{Reason: ReasonInternal}, fmt.Errorf("management: token: %w", err)
	}

	now := u.now().UTC()
	d := Domain{
		ID:                uuid.New(),
		TenantID:          tenantID,
		Host:              host,
		VerificationToken: token,
		TokenIssuedAt:     now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	saved, err := u.store.Insert(ctx, d)
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, Host: host, Action: "enroll", Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return EnrollResult{Reason: ReasonInternal}, fmt.Errorf("management: insert: %w", err)
	}
	u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: saved.ID, Host: host, Action: "enroll", Outcome: "ok", At: u.now()})
	return EnrollResult{
		Domain:    saved,
		TXTRecord: TXTRecordFor(host),
		TXTValue:  TXTValueFor(token),
	}, nil
}

// Verify resolves the TXT record for the domain and flips verified_at
// when the token matches. Idempotent: an already-verified row returns
// VerifyOutcome{Verified: true, Reason: ReasonAlreadyVerified} without
// re-hitting DNS.
//
// SIN-63104 hardenings:
//
//   - TTL gate before DNS: a token older than u.tokenTTL is rejected
//     with ReasonTokenExpired so a stale DNS takeover can't replay an
//     old enrollment.
//   - Compare-and-swap on MarkVerified: the store update is gated by
//     the expected verification_token so a concurrent rotation can't
//     race the flip-to-verified.
//   - TokenFingerprint on every token-bound audit event so ops can
//     correlate a denied:token_rotated event with the rotation that
//     caused it without leaking the token.
func (u *UseCase) Verify(ctx context.Context, tenantID, id uuid.UUID) (VerifyOutcome, error) {
	d, err := u.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, ErrStoreNotFound) {
			return VerifyOutcome{Reason: ReasonNotFound}, err
		}
		if errors.Is(err, ErrTenantMismatch) {
			return VerifyOutcome{Reason: ReasonForbidden}, err
		}
		return VerifyOutcome{Reason: ReasonInternal}, err
	}
	if d.VerifiedAt != nil {
		return VerifyOutcome{Domain: d, Verified: true, Reason: ReasonAlreadyVerified}, nil
	}
	now := u.now().UTC()
	fp := tokenFingerprint(d.VerificationToken)
	if now.Sub(d.TokenIssuedAt) > u.tokenTTL {
		u.logEvent(ctx, AuditEvent{
			TenantID:         tenantID,
			DomainID:         id,
			Host:             d.Host,
			Action:           "verify",
			Outcome:          "denied:token_expired",
			Reason:           ReasonTokenExpired,
			At:               now,
			TokenFingerprint: fp,
		})
		return VerifyOutcome{Domain: d, Reason: ReasonTokenExpired}, ErrTokenExpired
	}
	if u.dns == nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: id, Host: d.Host, Action: "verify", Outcome: "error", Reason: ReasonInternal, At: now, TokenFingerprint: fp})
		return VerifyOutcome{Domain: d, Reason: ReasonInternal}, errors.New("management: DNSChecker not configured")
	}
	res, err := u.dns.Check(ctx, d.Host, d.VerificationToken)
	if err != nil {
		reason := classifyValidationError(err)
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: id, Host: d.Host, Action: "verify", Outcome: "denied:" + reason.String(), Reason: reason, At: now, TokenFingerprint: fp})
		return VerifyOutcome{Domain: d, Reason: reason, Err: err}, err
	}
	saved, err := u.store.MarkVerified(ctx, id, d.VerificationToken, now, res.WithDNSSEC, res.LogID)
	if errors.Is(err, ErrTokenRotated) {
		u.logEvent(ctx, AuditEvent{
			TenantID:         tenantID,
			DomainID:         id,
			Host:             d.Host,
			Action:           "verify",
			Outcome:          "denied:token_rotated",
			Reason:           ReasonTokenRotated,
			At:               now,
			TokenFingerprint: fp,
		})
		return VerifyOutcome{Domain: d, Reason: ReasonTokenRotated, Err: err}, err
	}
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: id, Host: d.Host, Action: "verify", Outcome: "error", Reason: ReasonInternal, At: now, TokenFingerprint: fp})
		return VerifyOutcome{Domain: d, Reason: ReasonInternal}, fmt.Errorf("management: mark verified: %w", err)
	}
	u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: id, Host: d.Host, Action: "verify", Outcome: "ok", At: now, TokenFingerprint: fp})
	return VerifyOutcome{Domain: saved, Verified: true}, nil
}

// tokenFingerprint returns the first 16 hex chars (64 bits) of the
// SHA-256 of token. Enough entropy to correlate audit events across
// rotation races; never reversible. Empty input returns empty output so
// callers can pass through audit events that don't bind a token.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// SetPaused flips tls_paused_at. paused=true sets to now(); paused=false
// clears it. Both directions are reversible.
func (u *UseCase) SetPaused(ctx context.Context, tenantID, id uuid.UUID, paused bool) (Domain, error) {
	d, err := u.Get(ctx, tenantID, id)
	if err != nil {
		return Domain{}, err
	}
	var pausedAt *time.Time
	action := "resume"
	if paused {
		t := u.now().UTC()
		pausedAt = &t
		action = "pause"
	}
	saved, err := u.store.SetPaused(ctx, d.ID, pausedAt)
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: action, Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return Domain{}, fmt.Errorf("management: set paused: %w", err)
	}
	u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: action, Outcome: "ok", At: u.now()})
	return saved, nil
}

// Delete soft-deletes the row and triggers a 12-month slug-reservation
// lock per [SIN-62244]. Slug release errors do NOT roll the delete
// back; the audit log captures the gap so ops can re-run the release.
func (u *UseCase) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	d, err := u.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	now := u.now().UTC()
	if _, err := u.store.SoftDelete(ctx, d.ID, now); err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: "delete", Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return fmt.Errorf("management: soft delete: %w", err)
	}
	if u.slug != nil {
		if err := u.slug.ReleaseSlug(ctx, d.Host, tenantID); err != nil {
			u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: "delete", Outcome: "error", Reason: ReasonInternal, At: u.now()})
			return fmt.Errorf("management: release slug: %w", err)
		}
	}
	u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: "delete", Outcome: "ok", At: u.now()})
	return nil
}

// RegenerateToken issues a fresh verification token for an unverified domain.
// Returns ErrAlreadyVerified when the domain is already verified. The caller
// is responsible for RBAC; this method only checks tenant ownership.
func (u *UseCase) RegenerateToken(ctx context.Context, tenantID, id uuid.UUID) (Domain, error) {
	d, err := u.Get(ctx, tenantID, id)
	if err != nil {
		return Domain{}, err
	}
	if d.VerifiedAt != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: "regenerate_token", Outcome: "denied:" + ReasonAlreadyVerified.String(), Reason: ReasonAlreadyVerified, At: u.now()})
		return Domain{}, ErrAlreadyVerified
	}
	newToken, err := u.tokenGen()
	if err != nil {
		return Domain{}, fmt.Errorf("management: generate token: %w", err)
	}
	now := u.now().UTC()
	saved, err := u.store.RotateToken(ctx, d.ID, newToken, now)
	if err != nil {
		u.logEvent(ctx, AuditEvent{TenantID: tenantID, DomainID: d.ID, Host: d.Host, Action: "regenerate_token", Outcome: "error", Reason: ReasonInternal, At: u.now()})
		return Domain{}, fmt.Errorf("management: rotate token: %w", err)
	}
	u.logEvent(ctx, AuditEvent{
		TenantID:         tenantID,
		DomainID:         d.ID,
		Host:             d.Host,
		Action:           "regenerate_token",
		Outcome:          "ok",
		At:               u.now(),
		TokenFingerprint: tokenFingerprint(newToken),
	})
	return saved, nil
}

func (u *UseCase) logEvent(ctx context.Context, ev AuditEvent) {
	if u.audit != nil {
		u.audit.LogManagement(ctx, ev)
	}
}

// classifyValidationError maps a validator/DNS-checker error to a
// boundary reason. Concrete sentinels live in this package; downstream
// validators can wrap them.
func classifyValidationError(err error) Reason {
	switch {
	case err == nil:
		return ReasonNone
	case errors.Is(err, ErrInvalidHost):
		return ReasonInvalidHost
	case errors.Is(err, ErrPrivateIP):
		return ReasonPrivateIP
	case errors.Is(err, ErrTokenMismatch):
		return ReasonTokenMismatch
	case errors.Is(err, ErrSlugReserved):
		return ReasonSlugReserved
	default:
		return ReasonDNSResolutionFailed
	}
}

// TXTRecordFor returns the FQDN the tenant must add the TXT record to.
// Spec: `_crm-verify.<host>`.
func TXTRecordFor(host string) string {
	return "_crm-verify." + host
}

// TXTValueFor returns the value the TXT record must hold. Spec:
// `crm-verify=<token>`. The token is generated by the use-case's
// TokenGenerator.
func TXTValueFor(token string) string {
	return "crm-verify=" + token
}

// defaultTokenGen returns 32 random hex characters (128 bits of entropy).
// crypto/rand backed; never panics.
func defaultTokenGen() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// NormalizeHost lower-cases, trims, and validates the host as a syntactically
// well-formed FQDN per the F43/F45 rules:
//
//   - max 253 chars total
//   - max 63 chars per label, no leading/trailing hyphens
//   - at least one dot (rejects bare TLDs and "localhost")
//   - no IP literal (rejected by the dotted-decimal check)
//   - lowercase letters, digits, hyphens only
//
// The full DNS / private-IP rules live in customdomain/validation
// (SIN-62242). NormalizeHost is the cheap pre-flight that runs even
// when the validator port is nil (e.g. tests).
func NormalizeHost(raw string) (string, error) {
	host := strings.TrimSpace(strings.ToLower(raw))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", fmt.Errorf("%w: empty host", ErrInvalidHost)
	}
	if len(host) > 253 {
		return "", fmt.Errorf("%w: host too long (>253 chars)", ErrInvalidHost)
	}
	if !strings.Contains(host, ".") {
		return "", fmt.Errorf("%w: missing dot", ErrInvalidHost)
	}
	// IP-literal rejection: any all-digits-and-dots string fails fast.
	if isAllDigitsAndDots(host) {
		return "", fmt.Errorf("%w: IP literal not allowed", ErrInvalidHost)
	}
	for _, label := range strings.Split(host, ".") {
		if err := checkLabel(label); err != nil {
			return "", fmt.Errorf("%w: %s", ErrInvalidHost, err.Error())
		}
	}
	return host, nil
}

func checkLabel(label string) error {
	if label == "" {
		return errors.New("empty label")
	}
	if len(label) > 63 {
		return errors.New("label too long")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("label starts or ends with hyphen")
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return errors.New("label has invalid character")
		}
	}
	return nil
}

func isAllDigitsAndDots(host string) bool {
	for i := 0; i < len(host); i++ {
		c := host[i]
		if c == '.' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
