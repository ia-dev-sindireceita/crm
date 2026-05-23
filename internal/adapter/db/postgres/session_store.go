package postgres

// SIN-62213 IAM adapters. Each method opens its own WithTenant — the
// helper is non-composable (see its doc-comment) so iam.Service runs the
// IAM use case as a SEQUENCE of WithTenant transactions, never nested.
// That keeps argon2id verify out of any DB transaction (CPU-bound) and
// matches the V1 fallback approved in SIN-62213's design doc.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pericles-luz/crm/internal/iam"
)

// SessionStore is the pgx-backed adapter for iam.SessionStore. Construct
// with NewSessionStore(pool); the pool MUST be the app_runtime pool so
// RLS policies on the sessions table apply (see migrations/0006).
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore wraps pool. nil pool returns nil so callers see a clear
// nil-deref panic at first use rather than a silent "no rows" later.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	if pool == nil {
		return nil
	}
	return &SessionStore{pool: pool}
}

// Create inserts the session row inside a tenant-scoped transaction. The
// tenant_id is written explicitly — we do not rely on the RLS policy to
// fill it.
//
// SIN-63158: role and last_activity are written explicitly. An empty
// Session.Role is translated to RoleTenantCommon per the iam.Session.Role
// docstring contract (migration 0077 mirrors this default at the schema
// level; the adapter writes it on the way IN so a subsequent Get returns
// the canonical value regardless of which path filled the row). A zero
// LastActivity uses CreatedAt so the activity middleware does not reject
// the very first request after login.
//
// SIN-63222: csrf_token is written explicitly so the per-session CSRF
// value minted in iam.Login survives the next request — without this the
// RequireCSRF middleware always sees CSRFToken="" on re-hydration and
// rejects every POST/PATCH/DELETE with reason "csrf.token_missing".
func (s *SessionStore) Create(ctx context.Context, sess iam.Session) error {
	if sess.TenantID == uuid.Nil {
		return fmt.Errorf("postgres: SessionStore.Create: tenant id is nil")
	}
	role := sess.Role
	if role == "" {
		role = iam.RoleTenantCommon
	}
	lastActivity := sess.LastActivity
	if lastActivity.IsZero() {
		lastActivity = sess.CreatedAt
	}
	return WithTenant(ctx, s.pool, sess.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO sessions (id, tenant_id, user_id, expires_at, ip, user_agent, created_at, last_activity, role, csrf_token)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, sess.ID, sess.TenantID, sess.UserID, sess.ExpiresAt, ipForDB(sess.IPAddr), nullIfEmpty(sess.UserAgent), sess.CreatedAt, lastActivity, string(role), sess.CSRFToken)
		if err != nil {
			return fmt.Errorf("postgres: SessionStore.Create exec: %w", err)
		}
		return nil
	})
}

// Get reads a session by id, scoped to the tenant. Cross-tenant probes
// (a session id that belongs to another tenant) collapse to
// iam.ErrSessionNotFound — RLS hides the row, pgx returns ErrNoRows, we
// translate to the iam sentinel. An attacker therefore cannot
// distinguish "id does not exist anywhere" from "id exists in another
// tenant", which closes the cross-tenant enumeration channel.
func (s *SessionStore) Get(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	var out iam.Session
	err := WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var ip *netip.Prefix
		var ua *string
		var role string
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, user_id, expires_at, ip, user_agent, created_at, last_activity, role, csrf_token
			FROM sessions
			WHERE id = $1
		`, sessionID)
		if err := row.Scan(&out.ID, &out.TenantID, &out.UserID, &out.ExpiresAt, &ip, &ua, &out.CreatedAt, &out.LastActivity, &role, &out.CSRFToken); err != nil {
			return err
		}
		if ip != nil {
			out.IPAddr = net.IP(ip.Addr().AsSlice())
		}
		if ua != nil {
			out.UserAgent = *ua
		}
		out.Role = iam.Role(role)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	if err != nil {
		return iam.Session{}, fmt.Errorf("postgres: SessionStore.Get: %w", err)
	}
	return out, nil
}

// Delete removes the session by id, scoped to the tenant. Returns nil
// even when zero rows are affected: Logout is idempotent so a stale
// cookie hitting Delete twice does not surface a 5xx.
func (s *SessionStore) Delete(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
		if err != nil {
			return fmt.Errorf("postgres: SessionStore.Delete: %w", err)
		}
		return nil
	})
}

// DeleteExpired removes all sessions whose expires_at is in the past for
// the given tenant. Returns the number of rows deleted. The composite
// index sessions_tenant_id_expires_at_idx (created in
// migrations/0006_create_sessions.up.sql) keeps this cheap even at scale.
func (s *SessionStore) DeleteExpired(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var n int64
	err := WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: SessionStore.DeleteExpired: %w", err)
	}
	return n, nil
}

// Touch bumps last_activity to the supplied timestamp for (tenantID,
// sessionID). Returns iam.ErrSessionNotFound when the row is missing or
// hidden by RLS. Requires migration 0077_session_activity.
func (s *SessionStore) Touch(ctx context.Context, tenantID, sessionID uuid.UUID, lastActivity time.Time) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("postgres: SessionStore.Touch: tenant id is nil")
	}
	var found bool
	err := WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE sessions SET last_activity = $1 WHERE id = $2`,
			lastActivity, sessionID,
		)
		if err != nil {
			return err
		}
		found = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return fmt.Errorf("postgres: SessionStore.Touch: %w", err)
	}
	if !found {
		return iam.ErrSessionNotFound
	}
	return nil
}

// UserCredentialReader is the iam.UserCredentialReader adapter. It runs
// SELECT id, password_hash FROM users WHERE email = $1 inside a
// tenant-scoped transaction. RLS gates the row to the resolved tenant;
// no email leakage across tenants is possible.
type UserCredentialReader struct {
	pool *pgxpool.Pool
}

// NewUserCredentialReader wraps pool. nil pool returns nil so callers see
// a fast nil-deref panic at first use.
func NewUserCredentialReader(pool *pgxpool.Pool) *UserCredentialReader {
	if pool == nil {
		return nil
	}
	return &UserCredentialReader{pool: pool}
}

// LookupCredentials returns (uuid.Nil, "", nil) when no row matches —
// the iam.UserCredentialReader contract is "zero values on miss, error
// only for infra failures". This lets iam.Login distinguish "user does
// not exist" from "DB error" without an extra error value.
func (u *UserCredentialReader) LookupCredentials(ctx context.Context, tenantID uuid.UUID, email string) (uuid.UUID, string, error) {
	var (
		id   uuid.UUID
		hash string
	)
	err := WithTenant(ctx, u.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, password_hash FROM users WHERE email = $1
		`, email).Scan(&id, &hash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", nil
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("postgres: LookupCredentials: %w", err)
	}
	return id, hash, nil
}

// RoleByUser reads the raw users.role string for (tenantID, userID) and
// returns it as iam.Role. The value is NOT gated against an allowlist
// here — that is the use-case's job (iam.Service.resolveSessionRole
// applies the SIN-63340 §Item 1 tenant-scoped allowlist that maps any
// non-tenant value, including the high-risk 'master' string, down to
// RoleTenantCommon). A missing row returns the zero Role ("") with a
// nil error so Login can default the session role without 5xx-ing the
// request. RLS gates the SELECT on the tenant pool's app.tenant_id so
// cross-tenant probes resolve to the same zero-role / not-found path.
func (u *UserCredentialReader) RoleByUser(ctx context.Context, tenantID, userID uuid.UUID) (iam.Role, error) {
	var role string
	err := WithTenant(ctx, u.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT role FROM users WHERE id = $1
		`, userID).Scan(&role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return iam.Role(""), nil
	}
	if err != nil {
		return iam.Role(""), fmt.Errorf("postgres: RoleByUser: %w", err)
	}
	return iam.Role(role), nil
}

// ipForDB returns nil for an unset address so the inet column receives a
// SQL NULL. Otherwise it returns the address as a netip.Addr (pgx maps
// netip directly to the inet type without the textual round-trip).
func ipForDB(ip net.IP) any {
	if len(ip) == 0 {
		return nil
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return nil
	}
	return addr
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
