package usermfa

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pg "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/csrf"
)

// PendingsInner is the narrow surface PendingsBridge wraps. The
// postgres adapter *pg.TenantUserMFAPending satisfies it; tests
// inject a fake.
type PendingsInner interface {
	Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (pg.PendingMFASession, error)
	Get(ctx context.Context, id uuid.UUID) (pg.PendingMFASession, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// PendingsBridge adapts a PendingsInner (which returns
// pg.PendingMFASession) to the usermfa.PendingStore + PendingCreator
// ports (which return usermfa.Pending). The HTTP package owns the
// boundary type so it does not import pgx through the postgres type.
type PendingsBridge struct {
	inner PendingsInner
}

// NewPendingsBridge wraps a pending-store implementation.
func NewPendingsBridge(inner PendingsInner) *PendingsBridge {
	return &PendingsBridge{inner: inner}
}

// Create satisfies PendingCreator.
func (b *PendingsBridge) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (Pending, error) {
	row, err := b.inner.Create(ctx, userID, ttl, nextPath)
	if err != nil {
		return Pending{}, err
	}
	return toPending(row), nil
}

// Get satisfies PendingStore.
func (b *PendingsBridge) Get(ctx context.Context, id uuid.UUID) (Pending, error) {
	row, err := b.inner.Get(ctx, id)
	if err != nil {
		return Pending{}, err
	}
	return toPending(row), nil
}

// Delete satisfies PendingStore.
func (b *PendingsBridge) Delete(ctx context.Context, id uuid.UUID) error {
	return b.inner.Delete(ctx, id)
}

func toPending(row pg.PendingMFASession) Pending {
	return Pending{
		ID:        row.ID,
		UserID:    row.UserID,
		TenantID:  row.TenantID,
		ExpiresAt: row.ExpiresAt,
		NextPath:  row.NextPath,
	}
}

// RequirementsInner is the narrow surface RequirementsBridge wraps.
// *pg.TenantUserMFARequirement satisfies it.
type RequirementsInner interface {
	Load(ctx context.Context, userID uuid.UUID) (pg.UserMFARequirement, error)
}

// RequirementsBridge adapts a RequirementsInner to the
// usermfa.RequirementReader port.
type RequirementsBridge struct {
	inner RequirementsInner
}

// NewRequirementsBridge wraps a requirement-reader implementation.
func NewRequirementsBridge(inner RequirementsInner) *RequirementsBridge {
	return &RequirementsBridge{inner: inner}
}

// Load satisfies RequirementReader.
func (b *RequirementsBridge) Load(ctx context.Context, userID uuid.UUID) (Requirement, error) {
	row, err := b.inner.Load(ctx, userID)
	if err != nil {
		return Requirement{}, err
	}
	return Requirement{
		TOTPRequired: row.TOTPRequired,
		TOTPEnrolled: row.TOTPEnrolled,
	}, nil
}

// TenantSessionMinter satisfies SessionMinter by writing a fresh
// iam.Session row through the supplied SessionStore. Session timeouts
// and CSRF token minting match iam.Service.Login so the post-verify
// session is indistinguishable from a non-MFA login session.
type TenantSessionMinter struct {
	sessions iam.SessionStore
	ttl      time.Duration
	now      func() time.Time
}

// NewTenantSessionMinter wires a session minter around the iam port.
// A zero TTL falls back to DefaultSessionTTL.
func NewTenantSessionMinter(sessions iam.SessionStore, ttl time.Duration) (*TenantSessionMinter, error) {
	if sessions == nil {
		return nil, fmt.Errorf("usermfa: NewTenantSessionMinter: sessions is nil")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &TenantSessionMinter{sessions: sessions, ttl: ttl, now: time.Now}, nil
}

// WithClock overrides the time source for tests.
func (m *TenantSessionMinter) WithClock(now func() time.Time) *TenantSessionMinter {
	if now != nil {
		m.now = now
	}
	return m
}

// MintTenantSession creates a fresh tenant session row and returns it.
func (m *TenantSessionMinter) MintTenantSession(ctx context.Context, tenantID, userID uuid.UUID, ipAddr, userAgent string) (iam.Session, error) {
	id, err := iam.NewSessionID()
	if err != nil {
		return iam.Session{}, fmt.Errorf("usermfa: new session id: %w", err)
	}
	token, err := csrf.GenerateToken()
	if err != nil {
		return iam.Session{}, fmt.Errorf("usermfa: generate csrf token: %w", err)
	}
	now := m.now().UTC()
	sess := iam.Session{
		ID:           id,
		UserID:       userID,
		TenantID:     tenantID,
		ExpiresAt:    now.Add(m.ttl),
		CreatedAt:    now,
		IPAddr:       parseIP(ipAddr),
		UserAgent:    userAgent,
		LastActivity: now,
		Role:         iam.RoleTenantGerente,
		CSRFToken:    token,
	}
	if err := m.sessions.Create(ctx, sess); err != nil {
		return iam.Session{}, fmt.Errorf("usermfa: create session: %w", err)
	}
	return sess, nil
}

// PostgresUserLabel resolves the otpauth:// label for a tenant user by
// reading users.email under WithTenant.
type PostgresUserLabel struct {
	pool TenantPool
}

// TenantPool is the minimal pool surface needed by PostgresUserLabel.
// *pgxpool.Pool satisfies it. Defining the interface here keeps the
// adapter test-friendly without leaking the pool type.
type TenantPool = pg.TxBeginner

// NewPostgresUserLabel wraps a pool.
func NewPostgresUserLabel(pool TenantPool) (*PostgresUserLabel, error) {
	if pool == nil {
		return nil, fmt.Errorf("usermfa: NewPostgresUserLabel: pool is nil")
	}
	return &PostgresUserLabel{pool: pool}, nil
}

// LookupLabel returns users.email for the supplied principal.
func (a *PostgresUserLabel) LookupLabel(ctx context.Context, tenantID, userID uuid.UUID) (string, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil {
		return "", fmt.Errorf("usermfa: PostgresUserLabel.LookupLabel: zero id")
	}
	var email string
	err := pg.WithTenant(ctx, a.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	})
	if err != nil {
		return "", fmt.Errorf("usermfa: PostgresUserLabel.LookupLabel: %w", err)
	}
	return email, nil
}

// parseIP is a best-effort net.IP parser. Returns nil on unparseable
// input; iam.Session accepts nil and stamps the row accordingly.
func parseIP(s string) net.IP {
	if s == "" {
		return nil
	}
	return net.ParseIP(s)
}
