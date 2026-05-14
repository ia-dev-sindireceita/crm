// Package inbox is the pgx-backed adapter for the inbox.Repository
// and inbox.InboundDedupRepository ports (migration 0088:
// conversation + message + assignment + inbound_message_dedup).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods directly. Every tenant-scoped call routes through
// the sibling postgres.WithTenant helper so the RLS GUC app.tenant_id
// is set before reading or writing. The dedup ledger is intentionally
// NOT tenant-scoped — it consults a global UNIQUE(channel,
// channel_external_id) index before tenant context is fully resolved
// (ADR 0087 / SIN-62723).
//
// SIN-62729 (PR4 of the Fase 1 inbox stack, child of SIN-62193).
package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/inbox"
)

// Compile-time assertions: Store satisfies both ports. If a port grows
// or shrinks, the build fails here before any caller notices.
var (
	_ domain.Repository             = (*Store)(nil)
	_ domain.InboundDedupRepository = (*Store)(nil)
)

// pgUniqueViolation is the SQLSTATE for unique-violation. We translate
// the dedup-ledger constraint into ErrInboundAlreadyProcessed so the
// receive-inbound use case can collapse retries to a no-op.
const (
	pgUniqueViolation           = "23505"
	dedupExternalUniqConstraint = "inbound_message_dedup_channel_external_uniq"
)

// Store is the pgx-backed adapter. Construct via New(pool); the pool
// MUST be the app_runtime pool so the RLS policies on conversation /
// message / assignment apply.
type Store struct {
	pool postgres.TxBeginner
	now  func() time.Time
}

// New wraps pool and returns a ready-to-use Store. nil pool yields
// postgres.ErrNilPool.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// WithClock returns a copy of s that uses fn for every "now" read.
// Tests use it to make persistence deterministic. fn MUST NOT be nil.
func (s *Store) WithClock(fn func() time.Time) *Store {
	cp := *s
	cp.now = fn
	return &cp
}

func (s *Store) nowUTC() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// CreateConversation inserts a new conversation row inside a
// tenant-scoped transaction. The caller MUST build c via
// domain.NewConversation; passing a hand-crafted struct that fails
// the constructor's invariants is a programming error and bubbles up.
func (s *Store) CreateConversation(ctx context.Context, c *domain.Conversation) error {
	if c == nil {
		return fmt.Errorf("inbox/postgres: CreateConversation: nil conversation")
	}
	if c.TenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: CreateConversation: tenant id is nil")
	}
	if c.ID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: CreateConversation: conversation id is nil")
	}
	created := c.CreatedAt
	if created.IsZero() {
		created = s.nowUTC()
	}
	return postgres.WithTenant(ctx, s.pool, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO conversation
			  (id, tenant_id, contact_id, channel, state, assigned_user_id, last_message_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, c.ID, c.TenantID, c.ContactID, c.Channel, string(c.State),
			c.AssignedUserID, nullTime(c.LastMessageAt), created)
		if err != nil {
			return fmt.Errorf("inbox/postgres: CreateConversation: %w", err)
		}
		c.CreatedAt = created
		return nil
	})
}

// GetConversation returns the conversation under the given tenant
// scope. RLS-hidden rows from other tenants collapse to ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*domain.Conversation, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: GetConversation: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	var c *domain.Conversation
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, contact_id, channel, state,
			       assigned_user_id, last_message_at, created_at
			  FROM conversation
			 WHERE id = $1
		`, conversationID)
		conv, err := scanConversation(row)
		if err != nil {
			return err
		}
		c = conv
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: GetConversation: %w", err)
	}
	return c, nil
}

// FindOpenConversation returns the open conversation for
// (tenantID, contactID, channel) or ErrNotFound. State filter is
// hard-coded to 'open' — closed rows do not count toward "find or
// create" semantics.
func (s *Store) FindOpenConversation(ctx context.Context, tenantID, contactID uuid.UUID, channel string) (*domain.Conversation, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: FindOpenConversation: tenant id is nil")
	}
	if contactID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	var c *domain.Conversation
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, contact_id, channel, state,
			       assigned_user_id, last_message_at, created_at
			  FROM conversation
			 WHERE contact_id = $1 AND channel = $2 AND state = 'open'
			 ORDER BY created_at DESC
			 LIMIT 1
		`, contactID, channel)
		conv, err := scanConversation(row)
		if err != nil {
			return err
		}
		c = conv
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: FindOpenConversation: %w", err)
	}
	return c, nil
}

// SaveMessage inserts the message row and bumps the parent
// conversation's last_message_at. Both operations run in the same
// tenant-scoped transaction so a crash between them cannot leave the
// conversation's pointer behind.
func (s *Store) SaveMessage(ctx context.Context, m *domain.Message) error {
	if m == nil {
		return fmt.Errorf("inbox/postgres: SaveMessage: nil message")
	}
	if m.TenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SaveMessage: tenant id is nil")
	}
	if m.ID == uuid.Nil || m.ConversationID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SaveMessage: id/conversation id is nil")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = s.nowUTC()
	}
	return postgres.WithTenant(ctx, s.pool, m.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO message
			  (id, tenant_id, conversation_id, direction, body, status,
			   channel_external_id, sent_by_user_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`,
			m.ID, m.TenantID, m.ConversationID, string(m.Direction), m.Body,
			string(m.Status), nullString(m.ChannelExternalID),
			m.SentByUserID, created)
		if err != nil {
			return fmt.Errorf("inbox/postgres: SaveMessage insert: %w", err)
		}
		// Bump the parent's last_message_at, but only forward — out-of-order
		// carrier ACKs MUST NOT regress the inbox pointer.
		if _, err := tx.Exec(ctx, `
			UPDATE conversation
			   SET last_message_at = GREATEST(COALESCE(last_message_at, $2), $2)
			 WHERE id = $1
		`, m.ConversationID, created); err != nil {
			return fmt.Errorf("inbox/postgres: SaveMessage bump conversation: %w", err)
		}
		m.CreatedAt = created
		return nil
	})
}

// UpdateMessage persists status / channel_external_id changes on an
// existing message row. Returns ErrNotFound when nothing matches.
func (s *Store) UpdateMessage(ctx context.Context, m *domain.Message) error {
	if m == nil {
		return fmt.Errorf("inbox/postgres: UpdateMessage: nil message")
	}
	if m.TenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: UpdateMessage: tenant id is nil")
	}
	if m.ID == uuid.Nil {
		return domain.ErrNotFound
	}
	var affected int64
	err := postgres.WithTenant(ctx, s.pool, m.TenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE message
			   SET status = $1, channel_external_id = $2
			 WHERE id = $3
		`, string(m.Status), nullString(m.ChannelExternalID), m.ID)
		if err != nil {
			return err
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("inbox/postgres: UpdateMessage: %w", err)
	}
	if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Claim is the dedup-ledger half: insert the (channel, channelExternalID)
// row, translating a UNIQUE violation to ErrInboundAlreadyProcessed.
// NOT tenant-scoped — the receiver runs before tenant context exists.
func (s *Store) Claim(ctx context.Context, channel, channelExternalID string) error {
	if channel == "" || channelExternalID == "" {
		return fmt.Errorf("inbox/postgres: Claim: channel/externalID required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("inbox/postgres: Claim begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	_, err = tx.Exec(ctx, `
		INSERT INTO inbound_message_dedup (channel, channel_external_id)
		VALUES ($1, $2)
	`, channel, channelExternalID)
	if err != nil {
		if isDedupUniqueViolation(err) {
			return domain.ErrInboundAlreadyProcessed
		}
		return fmt.Errorf("inbox/postgres: Claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("inbox/postgres: Claim commit: %w", err)
	}
	committed = true
	return nil
}

// MarkProcessed flips processed_at on a previously-claimed row.
// Returns ErrNotFound when no row matches.
func (s *Store) MarkProcessed(ctx context.Context, channel, channelExternalID string) error {
	if channel == "" || channelExternalID == "" {
		return fmt.Errorf("inbox/postgres: MarkProcessed: channel/externalID required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("inbox/postgres: MarkProcessed begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	ct, err := tx.Exec(ctx, `
		UPDATE inbound_message_dedup
		   SET processed_at = now()
		 WHERE channel = $1
		   AND channel_external_id = $2
	`, channel, channelExternalID)
	if err != nil {
		return fmt.Errorf("inbox/postgres: MarkProcessed: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("inbox/postgres: MarkProcessed commit: %w", err)
	}
	committed = true
	return nil
}

func scanConversation(row pgx.Row) (*domain.Conversation, error) {
	var (
		id, tenantID, contactID uuid.UUID
		channel, state          string
		assignedUserID          *uuid.UUID
		lastMessageAt           *time.Time
		createdAt               time.Time
	)
	if err := row.Scan(&id, &tenantID, &contactID, &channel, &state,
		&assignedUserID, &lastMessageAt, &createdAt); err != nil {
		return nil, err
	}
	last := time.Time{}
	if lastMessageAt != nil {
		last = *lastMessageAt
	}
	return domain.HydrateConversation(
		id, tenantID, contactID, channel,
		domain.ConversationState(state), assignedUserID, last, createdAt,
	), nil
}

// isDedupUniqueViolation reports whether err is the
// inbound_message_dedup_channel_external_uniq violation. We match on
// constraint name (and message-substring fallback) so a future migration
// renaming the constraint trips a fast failure instead of silently
// flipping the error mapping.
func isDedupUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != pgUniqueViolation {
		return false
	}
	if pgErr.ConstraintName == dedupExternalUniqConstraint {
		return true
	}
	return false
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
