// Package mastersession is the pgx-backed adapter for the
// mastermfa.SessionStore and mastermfa.MasterSessionVerifiedAt ports
// over the master_session table (migration 0010).
//
// The package lives in its own directory under
// internal/adapter/db/postgres/ so the master-session storage
// surface is physically distinct from the tenant `sessions` adapter
// (postgres.SessionStore) — different roles, different RLS posture,
// different audit trail. Mixing them in one package leaks the
// "anyone can call us with either role" foot-gun.
//
// Every method runs under postgres.WithMasterOps so the
// master_ops_audit trigger from migration 0002 records the operator
// for every insert / update / delete. Create is the special case:
// at session-creation time the operator IS the master who just
// authenticated, so the actor on Create is the userID being passed
// in (rather than the per-adapter actorID captured at construction).
//
// SIN-62385 (PR1 of the SIN-62381 decomposition).
package mastersession

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// Compile-time assertions that Store satisfies both ports. If a port
// signature changes, the build fails here before it fails at the
// caller.
var (
	_ mastermfa.SessionStore            = (*Store)(nil)
	_ mastermfa.MasterSessionVerifiedAt = (*Store)(nil)
)

// Store is the master-scope adapter for the master_session table.
// Construct with New; the pool MUST be the app_master_ops pool so the
// master_ops_audit trigger fires on every statement.
//
// actorID names the human operator currently driving the master
// console for non-Create operations. Create is the exception — see
// the package doc-comment.
type Store struct {
	pool    postgres.TxBeginner
	actorID uuid.UUID
	// now is overridable for tests. Production callers leave it nil
	// and the adapter uses time.Now().UTC().
	now func() time.Time
}

// New validates inputs and returns an adapter ready for use. nil
// pool returns postgres.ErrNilPool so callers fail fast at
// construction rather than panic at first query. uuid.Nil actorID is
// rejected — a master operation without an audit actor would trip
// the trigger's master_ops_actor_user_id GUC guard at runtime, so
// reject it here for a louder error.
func New(pool *pgxpool.Pool, actorID uuid.UUID) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgres.ErrZeroActor
	}
	return &Store{pool: pool, actorID: actorID}, nil
}

// WithClock returns a copy of s that uses fn for every "now" read.
// Tests use it to make Create / Touch / MarkVerified deterministic.
// fn MUST NOT be nil.
func (s *Store) WithClock(fn func() time.Time) *Store {
	cp := *s
	cp.now = fn
	return &cp
}

// nowUTC is the production "now". Centralised so tests can swap it
// via WithClock without touching every method.
func (s *Store) nowUTC() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Create inserts a fresh master session for userID and returns the
// hydrated row. The audit actor for this single transaction is
// userID itself (see package doc-comment); every later operation on
// the row uses the per-adapter actorID instead.
//
// ttl MUST be > 0; a non-positive ttl would write a pre-expired row
// and is treated as a programming error.
func (s *Store) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration) (mastermfa.Session, error) {
	if userID == uuid.Nil {
		return mastermfa.Session{}, fmt.Errorf("mastersession: Create: userID is uuid.Nil")
	}
	if ttl <= 0 {
		return mastermfa.Session{}, fmt.Errorf("mastersession: Create: ttl must be > 0, got %s", ttl)
	}

	now := s.nowUTC()
	expires := now.Add(ttl)
	id := uuid.New()

	err := postgres.WithMasterOps(ctx, s.pool, userID, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `
			INSERT INTO master_session
			    (id, user_id, created_at, expires_at)
			VALUES
			    ($1, $2, $3, $4)
		`, id, userID, now, expires)
		return execErr
	})
	if err != nil {
		return mastermfa.Session{}, fmt.Errorf("mastersession: Create: %w", err)
	}
	return mastermfa.Session{
		ID:        id,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: expires,
	}, nil
}

// Get loads the session row by id. Translates pgx.ErrNoRows into
// mastermfa.ErrSessionNotFound; an existing-but-past expires_at is
// translated into mastermfa.ErrSessionExpired (and the row is
// returned alongside for observability — callers ignore it on the
// expired path but a future audit task may want it).
func (s *Store) Get(ctx context.Context, sessionID uuid.UUID) (mastermfa.Session, error) {
	if sessionID == uuid.Nil {
		return mastermfa.Session{}, mastermfa.ErrSessionNotFound
	}

	var (
		out       mastermfa.Session
		ip        *net.IPNet // pgx scans inet into *net.IPNet; nil = SQL NULL
		userAgent *string    // pointer so SQL NULL maps to nil
	)
	err := postgres.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, user_id, created_at, expires_at, mfa_verified_at, ip, user_agent
			  FROM master_session
			 WHERE id = $1
		`, sessionID).Scan(
			&out.ID,
			&out.UserID,
			&out.CreatedAt,
			&out.ExpiresAt,
			&out.MFAVerifiedAt,
			&ip,
			&userAgent,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return mastermfa.Session{}, mastermfa.ErrSessionNotFound
	}
	if err != nil {
		return mastermfa.Session{}, fmt.Errorf("mastersession: Get: %w", err)
	}
	if ip != nil {
		out.IP = ip.IP
	}
	if userAgent != nil {
		out.UserAgent = *userAgent
	}
	if !out.ExpiresAt.After(s.nowUTC()) {
		return out, mastermfa.ErrSessionExpired
	}
	return out, nil
}

// VerifiedAt is the narrow MasterSessionVerifiedAt port the
// RequireRecentMFA middleware (PR3) reads on every gated request.
// Returns the stored mfa_verified_at (the zero time if NULL — i.e.
// the session has only completed password auth). Returns
// mastermfa.ErrSessionNotFound on missing row. The expired-row case
// is intentionally NOT translated: the caller of VerifiedAt is the
// re-MFA freshness gate, which is downstream of the session-validity
// gate (RequireMasterAuth, PR2). If we surface "expired" here too, we
// duplicate the upstream check and risk drift if the two gates ever
// disagree about what "expired" means.
func (s *Store) VerifiedAt(ctx context.Context, sessionID uuid.UUID) (time.Time, error) {
	if sessionID == uuid.Nil {
		return time.Time{}, mastermfa.ErrSessionNotFound
	}
	var verifiedAt *time.Time
	err := postgres.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT mfa_verified_at FROM master_session WHERE id = $1`,
			sessionID,
		).Scan(&verifiedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, mastermfa.ErrSessionNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("mastersession: VerifiedAt: %w", err)
	}
	if verifiedAt == nil {
		return time.Time{}, nil
	}
	return *verifiedAt, nil
}

// Delete removes the session row. A missing row is intentionally
// not an error — see the SessionStore interface doc-comment.
func (s *Store) Delete(ctx context.Context, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return fmt.Errorf("mastersession: Delete: sessionID is uuid.Nil")
	}
	return postgres.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM master_session WHERE id = $1`, sessionID)
		if err != nil {
			return fmt.Errorf("mastersession: Delete exec: %w", err)
		}
		return nil
	})
}

// MarkVerified stamps mfa_verified_at = now() on the session row and
// returns the timestamp written. Returns mastermfa.ErrSessionNotFound
// if no row exists (zero RowsAffected).
func (s *Store) MarkVerified(ctx context.Context, sessionID uuid.UUID) (time.Time, error) {
	if sessionID == uuid.Nil {
		return time.Time{}, mastermfa.ErrSessionNotFound
	}
	now := s.nowUTC()
	var rows int64
	err := postgres.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		tag, execErr := tx.Exec(ctx,
			`UPDATE master_session SET mfa_verified_at = $1 WHERE id = $2`,
			now, sessionID,
		)
		if execErr != nil {
			return execErr
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("mastersession: MarkVerified: %w", err)
	}
	if rows == 0 {
		return time.Time{}, mastermfa.ErrSessionNotFound
	}
	return now, nil
}

// Touch extends expires_at to now + idleTTL on the session row.
// Returns mastermfa.ErrSessionNotFound if no row exists. idleTTL
// MUST be > 0; the master-auth middleware (PR2) is the sole caller
// and a zero idleTTL would silently retire the session on the next
// request.
func (s *Store) Touch(ctx context.Context, sessionID uuid.UUID, idleTTL time.Duration) error {
	if sessionID == uuid.Nil {
		return mastermfa.ErrSessionNotFound
	}
	if idleTTL <= 0 {
		return fmt.Errorf("mastersession: Touch: idleTTL must be > 0, got %s", idleTTL)
	}
	expires := s.nowUTC().Add(idleTTL)
	var rows int64
	err := postgres.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		tag, execErr := tx.Exec(ctx,
			`UPDATE master_session SET expires_at = $1 WHERE id = $2`,
			expires, sessionID,
		)
		if execErr != nil {
			return execErr
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("mastersession: Touch: %w", err)
	}
	if rows == 0 {
		return mastermfa.ErrSessionNotFound
	}
	return nil
}
