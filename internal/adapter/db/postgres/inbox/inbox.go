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
	"encoding/json"
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
	_ domain.ConversationReadModel  = (*Store)(nil)
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
//
// When m.Media is non-nil the JSON document is written to
// `message.media` at insert time (SIN-62848 AC #1: messages with
// attachments materialise with scan_status="pending" so the inbox
// view never renders an unscanned blob). Hash/Format are serialised
// only when populated so the persisted document stays minimal until
// the MediaScanner worker re-materialises the row.
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
	mediaJSON, err := marshalMessageMedia(m.Media)
	if err != nil {
		return fmt.Errorf("inbox/postgres: SaveMessage marshal media: %w", err)
	}
	return postgres.WithTenant(ctx, s.pool, m.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO message
			  (id, tenant_id, conversation_id, direction, body, status,
			   channel_external_id, sent_by_user_id, created_at, media)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			m.ID, m.TenantID, m.ConversationID, string(m.Direction), m.Body,
			string(m.Status), nullString(m.ChannelExternalID),
			m.SentByUserID, created, mediaJSON)
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

// FindMessageByChannelExternalID returns the message with the given
// (channel, channel_external_id) pair under the tenant scope. RLS
// hides rows from other tenants; an unknown id collapses to
// domain.ErrNotFound. Used by the status reconciler (WhatsApp PR8) to
// materialise the message before advancing its lifecycle state.
func (s *Store) FindMessageByChannelExternalID(ctx context.Context, tenantID uuid.UUID, channel, channelExternalID string) (*domain.Message, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: FindMessageByChannelExternalID: tenant id is nil")
	}
	if channel == "" || channelExternalID == "" {
		return nil, domain.ErrNotFound
	}
	var m *domain.Message
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT m.id, m.tenant_id, m.conversation_id, m.direction, m.body,
			       m.status, m.channel_external_id, m.sent_by_user_id, m.created_at,
			       m.media
			  FROM message m
			  JOIN conversation c ON c.id = m.conversation_id
			 WHERE c.channel = $1
			   AND m.channel_external_id = $2
			 LIMIT 1
		`, channel, channelExternalID)
		msg, err := scanMessage(row)
		if err != nil {
			return err
		}
		m = msg
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: FindMessageByChannelExternalID: %w", err)
	}
	return m, nil
}

// ListConversations returns up to `limit` conversations under the tenant
// scope, newest-last-message-first. The state filter is optional: pass
// the empty value to include both open and closed; pass
// domain.ConversationStateOpen to restrict. RLS-hidden rows from other
// tenants are simply absent from the result set.
func (s *Store) ListConversations(ctx context.Context, tenantID uuid.UUID, state domain.ConversationState, limit int) ([]*domain.Conversation, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversations: tenant id is nil")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("inbox/postgres: ListConversations: limit must be > 0")
	}
	const maxLimit = 200
	if limit > maxLimit {
		limit = maxLimit
	}
	var out []*domain.Conversation
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			rows pgx.Rows
			err  error
		)
		if state == "" {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, contact_id, channel, state,
				       assigned_user_id, last_message_at, created_at
				  FROM conversation
				 ORDER BY COALESCE(last_message_at, created_at) DESC, id ASC
				 LIMIT $1
			`, limit)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, contact_id, channel, state,
				       assigned_user_id, last_message_at, created_at
				  FROM conversation
				 WHERE state = $1
				 ORDER BY COALESCE(last_message_at, created_at) DESC, id ASC
				 LIMIT $2
			`, string(state), limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			conv, err := scanConversation(rows)
			if err != nil {
				return err
			}
			out = append(out, conv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversations: %w", err)
	}
	return out, nil
}

// ListConversationSummaries is the read-model (CQRS) path backing the
// GET /inbox list pane (SIN-64967). It returns up to `limit`
// ConversationListItem projections under the tenant scope,
// newest-last-message-first, narrowed by filter (state / channel /
// assigned user). The last-message snippet and direction are fetched in
// the SAME query via a LEFT JOIN LATERAL onto message (one row per
// conversation, ordered by created_at DESC) so the listing never issues
// a per-conversation follow-up query (no N+1). The primary row label
// (ContactDisplayName) is resolved in the SAME query via a JOIN on
// contact, falling back to the channel identifier
// (contact_channel_identity.external_id) and then the contact id so the
// UI never renders a bare UUID. The lateral lookup rides the existing
// message_conversation_created_idx (conversation_id, created_at DESC)
// from migration 0088; the outer ordering rides
// conversation_tenant_state_last_msg_idx. RLS hides other tenants' rows —
// on conversation, contact, and contact_channel_identity alike — so the
// listing and the label resolution can never cross tenants.
//
// The filters are expressed as nullable/sentinel predicates so a single
// prepared statement serves every combination: an empty state/channel
// disables that axis and a nil assigned id disables the "minhas" filter.
func (s *Store) ListConversationSummaries(ctx context.Context, tenantID uuid.UUID, filter domain.ConversationFilter, limit int) ([]domain.ConversationListItem, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversationSummaries: tenant id is nil")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("inbox/postgres: ListConversationSummaries: limit must be > 0")
	}
	const maxLimit = 200
	if limit > maxLimit {
		limit = maxLimit
	}
	var assigned *uuid.UUID
	if filter.AssignedUserID != uuid.Nil {
		id := filter.AssignedUserID
		assigned = &id
	}
	var out []domain.ConversationListItem
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.contact_id, c.channel, c.state,
			       c.assigned_user_id, c.last_message_at, c.created_at,
			       COALESCE(NULLIF(ct.display_name, ''), cci.external_id, c.contact_id::text) AS contact_label,
			       lm.body, lm.direction
			  FROM conversation c
			  JOIN contact ct ON ct.id = c.contact_id
			  LEFT JOIN contact_channel_identity cci
			         ON cci.contact_id = c.contact_id AND cci.channel = c.channel
			  LEFT JOIN LATERAL (
			      SELECT m.body, m.direction
			        FROM message m
			       WHERE m.conversation_id = c.id
			       ORDER BY m.created_at DESC, m.id DESC
			       LIMIT 1
			  ) lm ON true
			 WHERE ($1::text = '' OR c.state = $1)
			   AND ($2::text = '' OR c.channel = $2)
			   AND ($3::uuid IS NULL OR c.assigned_user_id = $3)
			 ORDER BY COALESCE(c.last_message_at, c.created_at) DESC, c.id ASC
			 LIMIT $4
		`, string(filter.State), filter.Channel, assigned, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanConversationListItem(rows)
			if err != nil {
				return err
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversationSummaries: %w", err)
	}
	return out, nil
}

// ListConversationsByContact returns up to `limit` conversations for a
// single contact under the tenant scope, newest-last-message-first. It
// backs the contact-detail "conversation history" panel (SIN-64976):
// the contacts read-side has a contact id and wants its threads, which
// the conversation_contact_idx index (migration 0088, on
// conversation(contact_id)) serves directly.
//
// This method is deliberately NOT part of the inbox.Repository port —
// only the postgres Store exposes it, and the contacts use-case declares
// a narrow reader interface satisfied structurally. Keeping it off the
// port avoids forcing every inbox.Repository fake to grow a method it
// does not exercise (mirrors the funnel Store.FindByID precedent used by
// GetConversationContext).
//
// A uuid.Nil tenant or contact yields an empty result with a clean error
// / nil rather than a cross-contact leak. limit must be > 0; the adapter
// clamps to the same upper bound as ListConversations.
func (s *Store) ListConversationsByContact(ctx context.Context, tenantID, contactID uuid.UUID, limit int) ([]*domain.Conversation, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversationsByContact: tenant id is nil")
	}
	if contactID == uuid.Nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, fmt.Errorf("inbox/postgres: ListConversationsByContact: limit must be > 0")
	}
	const maxLimit = 200
	if limit > maxLimit {
		limit = maxLimit
	}
	var out []*domain.Conversation
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, contact_id, channel, state,
			       assigned_user_id, last_message_at, created_at
			  FROM conversation
			 WHERE contact_id = $1
			 ORDER BY COALESCE(last_message_at, created_at) DESC, id ASC
			 LIMIT $2
		`, contactID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			conv, err := scanConversation(rows)
			if err != nil {
				return err
			}
			out = append(out, conv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListConversationsByContact: %w", err)
	}
	return out, nil
}

// ListMessages returns the messages for a conversation under the tenant
// scope, ordered oldest-first by created_at so the inbox renders the
// chat top→bottom. Returns ErrNotFound when the conversation itself is
// hidden by RLS (so callers cannot tell "RLS reject" from "empty thread"
// without first asserting GetConversation succeeded).
func (s *Store) ListMessages(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*domain.Message, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListMessages: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	var out []*domain.Message
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		// Guard: confirm the conversation exists under this tenant before
		// streaming messages, so a caller asking for an RLS-hidden id
		// gets ErrNotFound rather than a silent empty list.
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM conversation WHERE id = $1)
		`, conversationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrNotFound
		}
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, conversation_id, direction, body, status,
			       channel_external_id, sent_by_user_id, created_at, media
			  FROM message
			 WHERE conversation_id = $1
			 ORDER BY created_at ASC, id ASC
		`, conversationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListMessages: %w", err)
	}
	return out, nil
}

// GetMessage returns the single message identified by
// (tenantID, conversationID, messageID). Used by the realtime status
// partial (SIN-62736): the HTMX bubble polls /messages/:id/status and
// the handler reads through this method instead of ListMessages so the
// hot path is O(1) per poll. RLS-hidden rows and rows belonging to a
// different conversation collapse to domain.ErrNotFound — same posture
// as GetConversation so callers cannot distinguish cross-tenant
// existence.
func (s *Store) GetMessage(ctx context.Context, tenantID, conversationID, messageID uuid.UUID) (*domain.Message, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: GetMessage: tenant id is nil")
	}
	if conversationID == uuid.Nil || messageID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	var m *domain.Message
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, conversation_id, direction, body, status,
			       channel_external_id, sent_by_user_id, created_at, media
			  FROM message
			 WHERE id = $1
			   AND conversation_id = $2
		`, messageID, conversationID)
		msg, err := scanMessage(row)
		if err != nil {
			return err
		}
		m = msg
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: GetMessage: %w", err)
	}
	return m, nil
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

// scanConversationListItem materialises a read-model row produced by
// ListConversationSummaries. body/direction come from the lateral
// last-message lookup and are NULL when the conversation has no messages
// yet; the snippet is truncated here via domain.Snippet so the rune-safe
// truncation rule lives in one tested place.
func scanConversationListItem(row pgx.Row) (domain.ConversationListItem, error) {
	var (
		id, contactID  uuid.UUID
		channel, state string
		assignedUserID *uuid.UUID
		lastMessageAt  *time.Time
		createdAt      time.Time
		contactLabel   string
		body           *string
		direction      *string
	)
	if err := row.Scan(&id, &contactID, &channel, &state,
		&assignedUserID, &lastMessageAt, &createdAt, &contactLabel, &body, &direction); err != nil {
		return domain.ConversationListItem{}, err
	}
	item := domain.ConversationListItem{
		ID:                 id,
		ContactID:          contactID,
		Channel:            channel,
		State:              domain.ConversationState(state),
		AssignedUserID:     assignedUserID,
		CreatedAt:          createdAt,
		ContactDisplayName: contactLabel,
	}
	if lastMessageAt != nil {
		item.LastMessageAt = *lastMessageAt
	}
	if body != nil {
		item.LastMessageSnippet = domain.Snippet(*body)
	}
	if direction != nil {
		item.LastMessageDirection = domain.MessageDirection(*direction)
	}
	return item, nil
}

// scanMessage materialises a message row into the domain shape. It
// keeps the column order in lock-step with the ORDER BY in ListMessages
// so a future schema change trips a fast scan failure here rather than
// surprising the use case downstream.
//
// The media column (migration 0092) is projected onto domain.MessageMedia
// when non-null so the inbox UI can render the hide-flag placeholder for
// infected attachments ([SIN-62805] F2-05d). Adapters keep the parsing
// loose: unknown jsonb keys are tolerated; missing scan_status leaves
// the field empty (template treats it as "no scan yet").
func scanMessage(row pgx.Row) (*domain.Message, error) {
	var (
		id, tenantID, conversationID uuid.UUID
		direction, body, status      string
		channelExternalID            *string
		sentByUserID                 *uuid.UUID
		createdAt                    time.Time
		mediaJSON                    []byte
	)
	if err := row.Scan(&id, &tenantID, &conversationID, &direction, &body, &status,
		&channelExternalID, &sentByUserID, &createdAt, &mediaJSON); err != nil {
		return nil, err
	}
	cext := ""
	if channelExternalID != nil {
		cext = *channelExternalID
	}
	msg := domain.HydrateMessage(
		id, tenantID, conversationID,
		domain.MessageDirection(direction), body,
		domain.MessageStatus(status), cext, sentByUserID, createdAt,
	)
	if len(mediaJSON) > 0 {
		var media struct {
			Hash       string `json:"hash"`
			Format     string `json:"format"`
			ScanStatus string `json:"scan_status"`
		}
		if err := json.Unmarshal(mediaJSON, &media); err != nil {
			return nil, fmt.Errorf("inbox/postgres: parse media: %w", err)
		}
		msg.AttachMedia(media.Hash, media.Format, media.ScanStatus)
	}
	return msg, nil
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

// marshalMessageMedia serialises the domain MessageMedia into the
// `message.media` jsonb shape (see migration 0094). A nil pointer
// yields nil (NULL in Postgres) so text-only messages keep an empty
// media column. Empty fields are omitted so the persisted document
// stays minimal until the MediaScanner worker patches a verdict in.
func marshalMessageMedia(m *domain.MessageMedia) (any, error) {
	if m == nil {
		return nil, nil
	}
	payload := map[string]string{}
	if m.Hash != "" {
		payload["hash"] = m.Hash
	}
	if m.Format != "" {
		payload["format"] = m.Format
	}
	if m.ScanStatus != "" {
		payload["scan_status"] = m.ScanStatus
	}
	if len(payload) == 0 {
		return nil, nil
	}
	return json.Marshal(payload)
}
