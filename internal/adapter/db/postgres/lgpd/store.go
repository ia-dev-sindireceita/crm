// Package lgpd is the pgx-backed adapter for the lgpd domain ports
// (lgpd_deletion_request, contact-scoped export queries, anonymizing
// purge). Storage stays under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow pgx + pgxpool here.
//
// SIN-63186 / Fase 6 PR3.
package lgpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/lgpd"
)

// Compile-time port assertions.
var (
	_ domain.DeletionRepository = (*Store)(nil)
	_ domain.DeletionLister     = (*Store)(nil)
	_ domain.ExportRepository   = (*Store)(nil)
	_ domain.PurgeRepository    = (*Store)(nil)
)

// Store implements every lgpd domain port. The handler uses the
// runtime pool for tenant-scoped reads/writes (export queries +
// Upsert); the worker uses the master pool for cross-tenant
// finalisation (ListReady, MarkCompleted/Failed, PurgeContact). Get
// also routes through the master pool because the worker is its main
// caller and it needs to see rows across every tenant.
type Store struct {
	runtimePool postgres.TxBeginner
	masterPool  postgres.TxBeginner
}

// New wraps both pools. A nil pool yields postgres.ErrNilPool so
// cmd/server fails fast on misconfiguration.
func New(runtimePool, masterPool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil || masterPool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{runtimePool: runtimePool, masterPool: masterPool}, nil
}

// Upsert satisfies DeletionRepository. The unique partial index
// lgpd_deletion_request_pending_uniq (tenant_id, contact_id WHERE
// status='pending') turns a duplicate POST into an UPDATE of the
// existing pending row — refreshing justification, requested_by, and
// retention_until without ever creating a second open request.
func (s *Store) Upsert(ctx context.Context, req domain.DeletionRequest) (domain.DeletionRequest, error) {
	if err := req.Validate(); err != nil {
		return domain.DeletionRequest{}, err
	}
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	req.UpdatedAt = time.Now().UTC()

	var out domain.DeletionRequest
	err := postgres.WithTenant(ctx, s.runtimePool, req.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO lgpd_deletion_request (
				id, tenant_id, contact_id, requested_by_user_id,
				justification, status, retention_until,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, contact_id) WHERE status = 'pending'
			DO UPDATE SET
				requested_by_user_id = EXCLUDED.requested_by_user_id,
				justification        = EXCLUDED.justification,
				retention_until      = EXCLUDED.retention_until,
				updated_at           = EXCLUDED.updated_at
			RETURNING id, tenant_id, contact_id, requested_by_user_id,
			          justification, status, retention_until,
			          completed_at, created_at, updated_at
		`, req.ID, req.TenantID, req.ContactID, nullableUUID(req.RequestedByUserID),
			req.Justification, string(req.Status), req.RetentionUntil,
			req.CreatedAt, req.UpdatedAt)
		return scanDeletionRow(row, &out)
	})
	if err != nil {
		return domain.DeletionRequest{}, fmt.Errorf("lgpd/postgres: upsert: %w", err)
	}
	return out, nil
}

// Get satisfies DeletionRepository. The worker calls Get during retry
// to confirm the row is still pending before re-running the purge.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (domain.DeletionRequest, error) {
	if id == uuid.Nil {
		return domain.DeletionRequest{}, errors.New("lgpd/postgres: zero id")
	}
	// Get is called by the worker (master_ops) — go through that role.
	var out domain.DeletionRequest
	err := postgres.WithMasterOps(ctx, s.masterPool, masterActorSentinel, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, contact_id, requested_by_user_id,
			       justification, status, retention_until,
			       completed_at, created_at, updated_at
			  FROM lgpd_deletion_request
			 WHERE id = $1
		`, id)
		err := scanDeletionRow(row, &out)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDeletionRequestNotFound
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrDeletionRequestNotFound) {
			return domain.DeletionRequest{}, err
		}
		return domain.DeletionRequest{}, fmt.Errorf("lgpd/postgres: get: %w", err)
	}
	return out, nil
}

// ListReady returns up to `limit` pending rows whose retention_until
// has elapsed. The worker takes them in created_at order so the
// oldest backlog item is handled first.
func (s *Store) ListReady(ctx context.Context, at time.Time, limit int) ([]domain.DeletionRequest, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.DeletionRequest
	err := postgres.WithMasterOps(ctx, s.masterPool, masterActorSentinel, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, contact_id, requested_by_user_id,
			       justification, status, retention_until,
			       completed_at, created_at, updated_at
			  FROM lgpd_deletion_request
			 WHERE status = 'pending'
			   AND retention_until <= $1
			 ORDER BY created_at ASC
			 LIMIT $2
		`, at, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.DeletionRequest
			if err := scanDeletionRow(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list ready: %w", err)
	}
	return out, nil
}

// ListByTenant satisfies DeletionRepository. Reads through the runtime
// pool (RLS scopes by app.tenant_id) so an admin user only sees their
// own tenant's queue — the cross-tenant view is reserved for the
// master-ops worker (ListReady). An empty status (DeletionStatus(""))
// returns every row; a controlled-vocabulary value applies the filter.
// SIN-63191 / Fase 6 PR4.
func (s *Store) ListByTenant(ctx context.Context, tenant uuid.UUID, status domain.DeletionStatus, limit int) ([]domain.DeletionRequest, error) {
	if tenant == uuid.Nil {
		return nil, errors.New("lgpd/postgres: zero tenant")
	}
	if limit <= 0 {
		limit = 100
	}
	var out []domain.DeletionRequest
	err := postgres.WithTenant(ctx, s.runtimePool, tenant, func(tx pgx.Tx) error {
		var (
			rows pgx.Rows
			err  error
		)
		if status == "" {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, contact_id, requested_by_user_id,
				       justification, status, retention_until,
				       completed_at, created_at, updated_at
				  FROM lgpd_deletion_request
				 ORDER BY created_at DESC
				 LIMIT $1
			`, limit)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, contact_id, requested_by_user_id,
				       justification, status, retention_until,
				       completed_at, created_at, updated_at
				  FROM lgpd_deletion_request
				 WHERE status = $1
				 ORDER BY created_at DESC
				 LIMIT $2
			`, string(status), limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.DeletionRequest
			if err := scanDeletionRow(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list by tenant: %w", err)
	}
	return out, nil
}

// MarkCompleted satisfies DeletionRepository.
func (s *Store) MarkCompleted(ctx context.Context, id uuid.UUID, at time.Time) error {
	return postgres.WithMasterOps(ctx, s.masterPool, masterActorSentinel, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE lgpd_deletion_request
			   SET status = 'completed',
			       completed_at = $2,
			       updated_at = $2
			 WHERE id = $1
			   AND status = 'pending'
		`, id, at)
		return err
	})
}

// MarkFailed satisfies DeletionRepository.
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return postgres.WithMasterOps(ctx, s.masterPool, masterActorSentinel, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE lgpd_deletion_request
			   SET status = 'failed',
			       updated_at = $2
			 WHERE id = $1
			   AND status = 'pending'
		`, id, at)
		return err
	})
}

// GetContact satisfies ExportRepository.
func (s *Store) GetContact(ctx context.Context, tenantID, contactID uuid.UUID) (domain.ExportContact, error) {
	var c domain.ExportContact
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, display_name, created_at, updated_at
			  FROM contact
			 WHERE id = $1
		`, contactID)
		err := row.Scan(&c.ID, &c.TenantID, &c.DisplayName, &c.CreatedAt, &c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDeletionRequestNotFound
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrDeletionRequestNotFound) {
			return domain.ExportContact{}, err
		}
		return domain.ExportContact{}, fmt.Errorf("lgpd/postgres: get contact: %w", err)
	}
	return c, nil
}

// ListIdentities satisfies ExportRepository.
func (s *Store) ListIdentities(ctx context.Context, tenantID, contactID uuid.UUID) ([]domain.ExportIdentity, error) {
	var out []domain.ExportIdentity
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, channel, external_id, created_at
			  FROM contact_channel_identity
			 WHERE contact_id = $1
			 ORDER BY created_at ASC
		`, contactID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportIdentity
			if err := rows.Scan(&it.ID, &it.Channel, &it.ExternalID, &it.CreatedAt); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list identities: %w", err)
	}
	return out, nil
}

// ListConversations satisfies ExportRepository.
func (s *Store) ListConversations(ctx context.Context, tenantID, contactID uuid.UUID) ([]domain.ExportConversation, error) {
	var out []domain.ExportConversation
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, channel, state, last_message_at, created_at
			  FROM conversation
			 WHERE contact_id = $1
			 ORDER BY created_at ASC
		`, contactID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportConversation
			var last *time.Time
			if err := rows.Scan(&it.ID, &it.Channel, &it.State, &last, &it.CreatedAt); err != nil {
				return err
			}
			it.LastMessageAt = last
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list conversations: %w", err)
	}
	return out, nil
}

// ListMessages satisfies ExportRepository. We scope by conversation
// rather than tenant alone so a contact deleted by mistake is not
// confused with messages from a different contact in the same tenant.
func (s *Store) ListMessages(ctx context.Context, tenantID, contactID uuid.UUID) ([]domain.ExportMessage, error) {
	var out []domain.ExportMessage
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.conversation_id, m.direction, m.body, m.status,
			       m.channel_external_id, m.media, m.created_at
			  FROM message m
			  JOIN conversation c ON c.id = m.conversation_id
			 WHERE c.contact_id = $1
			 ORDER BY m.created_at ASC
		`, contactID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportMessage
			var external *string
			var media *string
			var mediaRaw []byte
			if err := rows.Scan(&it.ID, &it.ConversationID, &it.Direction, &it.Body, &it.Status, &external, &mediaRaw, &it.CreatedAt); err != nil {
				return err
			}
			if len(mediaRaw) > 0 {
				// pgx returns jsonb as []byte; render as string so the
				// JSON encoder treats it as a verbatim text field.
				s := string(mediaRaw)
				media = &s
			}
			it.ChannelExternalID = external
			it.Media = media
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list messages: %w", err)
	}
	return out, nil
}

// ListBillingEvents satisfies ExportRepository. We surface only the
// security-audit rows whose target jsonb references the requested
// contact_id — billing rows that name a different contact stay out of
// the export.
func (s *Store) ListBillingEvents(ctx context.Context, tenantID, contactID uuid.UUID) ([]domain.ExportBillingEvent, error) {
	var out []domain.ExportBillingEvent
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, event_type, target::text, occurred_at
			  FROM audit_log_security
			 WHERE tenant_id = $1
			   AND target ? 'contact_id'
			   AND target->>'contact_id' = $2
			 ORDER BY occurred_at ASC
		`, tenantID, contactID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportBillingEvent
			if err := rows.Scan(&it.ID, &it.EventType, &it.Target, &it.OccurredAt); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list billing events: %w", err)
	}
	return out, nil
}

// ListConsents satisfies ExportRepository.
func (s *Store) ListConsents(ctx context.Context, tenantID uuid.UUID) ([]domain.ExportConsent, error) {
	var out []domain.ExportConsent
	err := postgres.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, scope_kind, scope_id, anonymizer_version, prompt_version, accepted_at
			  FROM ai_policy_consent
			 ORDER BY accepted_at ASC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportConsent
			if err := rows.Scan(&it.ID, &it.ScopeKind, &it.ScopeID, &it.AnonymizerVersion, &it.PromptVersion, &it.AcceptedAt); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("lgpd/postgres: list consents: %w", err)
	}
	return out, nil
}

// PurgeContact satisfies PurgeRepository. Runs as master_ops so the
// cross-table cascade is audited via master_ops_audit_trigger.
//
// Order matters: drop child rows that have FK ON DELETE CASCADE first
// would be wasted (CASCADE handles them) — but the contact row update
// must run BEFORE the contact delete because we need a row to
// anonymise. The actual implementation:
//
//  1. Anonymise the contact display_name (keep id + tenant_id for FK
//     referential integrity in fiscal audit rows).
//  2. Delete contact_channel_identity rows (channel handles count as
//     personal data; no fiscal duty to retain them).
//  3. Conversations + messages cascade-delete from the contact FK in
//     migration 0088. We do an explicit DELETE to keep the audit row
//     concise (one row per table touched).
func (s *Store) PurgeContact(ctx context.Context, tenantID, contactID uuid.UUID) error {
	return postgres.WithMasterOps(ctx, s.masterPool, masterActorSentinel, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE contact
			   SET display_name = '[anonymised:lgpd]',
			       updated_at = now()
			 WHERE id = $1 AND tenant_id = $2
		`, contactID, tenantID); err != nil {
			return fmt.Errorf("anonymise contact: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM contact_channel_identity WHERE contact_id = $1 AND tenant_id = $2`,
			contactID, tenantID); err != nil {
			return fmt.Errorf("delete identities: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM message
			 WHERE tenant_id = $2
			   AND conversation_id IN (
			     SELECT id FROM conversation WHERE contact_id = $1 AND tenant_id = $2
			   )
		`, contactID, tenantID); err != nil {
			return fmt.Errorf("delete messages: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM conversation WHERE contact_id = $1 AND tenant_id = $2`,
			contactID, tenantID); err != nil {
			return fmt.Errorf("delete conversations: %w", err)
		}
		return nil
	})
}

// masterActorSentinel is the actor uuid the worker uses when it runs
// without a human user. The master_ops_audit trigger requires a
// non-nil actor; this sentinel makes the worker's writes greppable in
// master_ops_audit.
var masterActorSentinel = uuid.MustParse("00000000-0000-0000-0000-000000000063")

// scanDeletionRow scans a single row whose column list matches the
// SELECT and RETURNING clauses above into dst.
func scanDeletionRow(src interface {
	Scan(...any) error
}, dst *domain.DeletionRequest) error {
	var requestedBy *uuid.UUID
	var completedAt *time.Time
	var status string
	if err := src.Scan(&dst.ID, &dst.TenantID, &dst.ContactID, &requestedBy,
		&dst.Justification, &status, &dst.RetentionUntil,
		&completedAt, &dst.CreatedAt, &dst.UpdatedAt); err != nil {
		return err
	}
	if requestedBy != nil {
		dst.RequestedByUserID = *requestedBy
	}
	dst.Status = domain.DeletionStatus(status)
	dst.CompletedAt = completedAt
	return nil
}

// nullableUUID returns nil for uuid.Nil so the column is written as
// NULL instead of as the zero uuid (which would fail the FK to users).
func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// jsonbToString is unused today but documents how to convert pgx jsonb
// scans to []byte → string when a future caller needs it.
var _ = json.Marshal
