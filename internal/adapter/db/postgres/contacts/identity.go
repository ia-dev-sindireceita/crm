// Package contacts — pgx adapter for contacts.IdentityRepository.
// Backed by migration 0092 (identity + contact_identity_link tables).
package contacts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/contacts"
)

// Compile-time assertion.
var _ contacts.IdentityRepository = (*IdentityStore)(nil)

// IdentityStore is the pgx-backed adapter for contacts.IdentityRepository.
// Construct via NewIdentityStore. Uses the app_runtime pool so RLS
// policies on identity / contact_identity_link apply.
//
// Feature flag feature.identity_merge.enabled: when mergeEnabled is
// false, Resolve performs only the direct (channel, externalID) lookup
// and skips phone/email cross-channel merging. Default: true.
type IdentityStore struct {
	pool         postgres.TxBeginner
	mergeEnabled bool
	now          func() time.Time
}

// NewIdentityStore wraps pool and returns a ready-to-use IdentityStore
// with mergeEnabled=true. A nil pool returns postgres.ErrNilPool.
func NewIdentityStore(pool *pgxpool.Pool) (*IdentityStore, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &IdentityStore{pool: pool, mergeEnabled: true}, nil
}

// WithMergeEnabled returns a copy of s with the merge feature flag set.
// Pass false to disable cross-channel phone/email merging (tenant gate
// for feature.identity_merge.enabled).
func (s *IdentityStore) WithMergeEnabled(v bool) *IdentityStore {
	cp := *s
	cp.mergeEnabled = v
	return &cp
}

// WithClock returns a copy with a pinned clock (for tests).
func (s *IdentityStore) WithClock(fn func() time.Time) *IdentityStore {
	cp := *s
	cp.now = fn
	return &cp
}

func (s *IdentityStore) nowUTC() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Resolve finds or creates the Identity for an inbound contact signal.
// phone and email may be empty; their lookups are skipped when empty.
// When mergeEnabled=false, only the (channel, externalID) lookup runs.
func (s *IdentityStore) Resolve(
	ctx context.Context,
	tenantID uuid.UUID,
	channel, externalID, phone, email string,
) (*contacts.Identity, *contacts.MergeProposal, error) {
	if tenantID == uuid.Nil {
		return nil, nil, fmt.Errorf("contacts/identity: Resolve: tenant id is nil")
	}

	phone = strings.TrimSpace(phone)
	email = strings.ToLower(strings.TrimSpace(email))

	var identity *contacts.Identity
	var proposal *contacts.MergeProposal

	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		candidates, err := s.lookupCandidates(ctx, tx, tenantID, channel, externalID, phone, email)
		if err != nil {
			return err
		}

		decision := contacts.DecideMerge(candidates)

		switch decision.Action {
		case contacts.MergeActionNew:
			id, err := s.createIdentity(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			if err := s.linkContact(ctx, tx, tenantID, id.ID, externalID, channel, contacts.LinkReasonExternalID); err != nil {
				return err
			}
			loaded, err := s.loadIdentity(ctx, tx, tenantID, id.ID)
			if err != nil {
				return err
			}
			identity = loaded

		case contacts.MergeActionLink:
			id, err := s.loadIdentity(ctx, tx, tenantID, decision.TargetID)
			if err != nil {
				return err
			}
			identity = id

		case contacts.MergeActionMerge:
			for _, srcID := range decision.SourceIDs {
				if err := s.mergeInTx(ctx, tx, tenantID, srcID, decision.TargetID); err != nil {
					return err
				}
			}
			id, err := s.loadIdentity(ctx, tx, tenantID, decision.TargetID)
			if err != nil {
				return err
			}
			identity = id

		case contacts.MergeActionPropose:
			// Cannot auto-merge: emit a proposal and use the first leader's
			// identity as the provisional result so the caller can continue.
			// The proposal is persisted by the caller if they wish; here we
			// build the struct and return it with the first candidate identity.
			id, err := s.loadIdentity(ctx, tx, tenantID, candidates[0].IdentityID)
			if err != nil {
				return err
			}
			identity = id
			proposal = &contacts.MergeProposal{
				ID:        uuid.New(),
				TenantID:  tenantID,
				SourceID:  candidates[1].IdentityID,
				TargetID:  candidates[0].IdentityID,
				Reason:    "leader conflict during resolve",
				CreatedAt: s.nowUTC(),
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("contacts/identity: Resolve: %w", err)
	}
	return identity, proposal, nil
}

// Merge absorbs sourceID into targetID. All contact_identity_link rows
// pointing at sourceID are repointed to targetID. sourceID.merged_into_id
// is set to targetID. Uses SELECT … FOR UPDATE to prevent concurrent races.
func (s *IdentityStore) Merge(
	ctx context.Context,
	tenantID, sourceID, targetID uuid.UUID,
	reason string,
) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("contacts/identity: Merge: tenant id is nil")
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return s.mergeInTx(ctx, tx, tenantID, sourceID, targetID)
	})
	if err != nil {
		return fmt.Errorf("contacts/identity: Merge: %w", err)
	}
	return nil
}

// FindByContactID resolves contactID's current Identity and hydrates its
// Links. Used by the split UI (SIN-62799 / F2-13) so the operator sees
// every contact tied to the merged identity. Returns contacts.ErrNotFound
// when the contact has no contact_identity_link row.
func (s *IdentityStore) FindByContactID(
	ctx context.Context, tenantID, contactID uuid.UUID,
) (*contacts.Identity, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("contacts/identity: FindByContactID: tenant id is nil")
	}
	var identity *contacts.Identity
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var identityID uuid.UUID
		row := tx.QueryRow(ctx, `
			SELECT identity_id FROM contact_identity_link
			 WHERE contact_id = $1
			 LIMIT 1
		`, contactID)
		if err := row.Scan(&identityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return contacts.ErrNotFound
			}
			return err
		}
		loaded, err := s.loadIdentity(ctx, tx, tenantID, identityID)
		if err != nil {
			return err
		}
		identity = loaded
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("contacts/identity: FindByContactID: %w", err)
	}
	return identity, nil
}

// Split removes linkID from contact_identity_link and creates a new
// Identity for the orphaned contact.
func (s *IdentityStore) Split(ctx context.Context, tenantID, linkID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("contacts/identity: Split: tenant id is nil")
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var contactID uuid.UUID
		row := tx.QueryRow(ctx, `
			SELECT contact_id FROM contact_identity_link
			 WHERE id = $1
			   FOR UPDATE
		`, linkID)
		if err := row.Scan(&contactID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return contacts.ErrNotFound
			}
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM contact_identity_link WHERE id = $1
		`, linkID); err != nil {
			return err
		}
		newID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity (id, tenant_id) VALUES ($1, $2)
		`, newID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason)
			VALUES ($1, $2, $3, 'manual')
		`, tenantID, newID, contactID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("contacts/identity: Split: %w", err)
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

func (s *IdentityStore) lookupCandidates(
	ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	channel, externalID, phone, email string,
) ([]contacts.MergeCandidate, error) {
	// Direct (channel, externalID) lookup — always performed.
	var candidates []contacts.MergeCandidate
	if id, hasLeader, err := s.lookupByChannelExternal(ctx, tx, tenantID, channel, externalID); err == nil {
		candidates = append(candidates, contacts.MergeCandidate{
			IdentityID: id, Reason: contacts.LinkReasonExternalID, HasLeader: hasLeader,
		})
	} else if !errors.Is(err, contacts.ErrNotFound) {
		return nil, err
	}

	if !s.mergeEnabled {
		return candidates, nil
	}

	// Phone lookup.
	if phone != "" {
		if id, hasLeader, err := s.lookupByPhone(ctx, tx, tenantID, phone); err == nil {
			candidates = append(candidates, contacts.MergeCandidate{
				IdentityID: id, Reason: contacts.LinkReasonPhone, HasLeader: hasLeader,
			})
		} else if !errors.Is(err, contacts.ErrNotFound) {
			return nil, err
		}
	}

	// Email lookup.
	if email != "" {
		if id, hasLeader, err := s.lookupByEmail(ctx, tx, tenantID, email); err == nil {
			candidates = append(candidates, contacts.MergeCandidate{
				IdentityID: id, Reason: contacts.LinkReasonEmail, HasLeader: hasLeader,
			})
		} else if !errors.Is(err, contacts.ErrNotFound) {
			return nil, err
		}
	}
	return candidates, nil
}

// lookupByChannelExternal finds the identity linked to a (channel, externalID) pair.
func (s *IdentityStore) lookupByChannelExternal(
	ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, channel, externalID string,
) (uuid.UUID, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT cil.identity_id,
		       EXISTS(
		         SELECT 1 FROM assignment_history ah
		          JOIN conversation cv ON cv.id = ah.conversation_id
		          JOIN contact c ON c.id = cil.contact_id
		         WHERE cv.contact_id = c.id
		           AND ah.tenant_id = $1
		         LIMIT 1
		       ) AS has_leader
		  FROM contact_channel_identity cci
		  JOIN contact_identity_link cil ON cil.contact_id = cci.contact_id
		 WHERE cci.channel = $2
		   AND cci.external_id = $3
	`, tenantID, channel, externalID)
	var id uuid.UUID
	var hasLeader bool
	if err := row.Scan(&id, &hasLeader); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, contacts.ErrNotFound
		}
		return uuid.Nil, false, err
	}
	return id, hasLeader, nil
}

// lookupByPhone finds the identity whose contact has phone matching in
// contact_channel_identity where channel='whatsapp' (E.164 normalised).
func (s *IdentityStore) lookupByPhone(
	ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, phone string,
) (uuid.UUID, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT cil.identity_id,
		       EXISTS(
		         SELECT 1 FROM assignment_history ah
		          JOIN conversation cv ON cv.id = ah.conversation_id
		          JOIN contact c ON c.id = cil.contact_id
		         WHERE cv.contact_id = c.id
		           AND ah.tenant_id = $1
		         LIMIT 1
		       ) AS has_leader
		  FROM contact_channel_identity cci
		  JOIN contact_identity_link cil ON cil.contact_id = cci.contact_id
		 WHERE cci.channel = 'whatsapp'
		   AND cci.external_id = $2
		 LIMIT 1
	`, tenantID, phone)
	var id uuid.UUID
	var hasLeader bool
	if err := row.Scan(&id, &hasLeader); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, contacts.ErrNotFound
		}
		return uuid.Nil, false, err
	}
	return id, hasLeader, nil
}

// lookupByEmail finds the identity whose contact display_name is not
// used for email; instead we look in contact_channel_identity where
// channel='email'.
func (s *IdentityStore) lookupByEmail(
	ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email string,
) (uuid.UUID, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT cil.identity_id,
		       EXISTS(
		         SELECT 1 FROM assignment_history ah
		          JOIN conversation cv ON cv.id = ah.conversation_id
		          JOIN contact c ON c.id = cil.contact_id
		         WHERE cv.contact_id = c.id
		           AND ah.tenant_id = $1
		         LIMIT 1
		       ) AS has_leader
		  FROM contact_channel_identity cci
		  JOIN contact_identity_link cil ON cil.contact_id = cci.contact_id
		 WHERE cci.channel = 'email'
		   AND cci.external_id = $2
		 LIMIT 1
	`, tenantID, email)
	var id uuid.UUID
	var hasLeader bool
	if err := row.Scan(&id, &hasLeader); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, contacts.ErrNotFound
		}
		return uuid.Nil, false, err
	}
	return id, hasLeader, nil
}

func (s *IdentityStore) createIdentity(
	ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
) (*contacts.Identity, error) {
	id := uuid.New()
	now := s.nowUTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity (id, tenant_id, created_at) VALUES ($1, $2, $3)
	`, id, tenantID, now); err != nil {
		return nil, err
	}
	return &contacts.Identity{ID: id, TenantID: tenantID, CreatedAt: now}, nil
}

func (s *IdentityStore) linkContact(
	ctx context.Context, tx pgx.Tx,
	tenantID, identityID uuid.UUID,
	contactID, channel string,
	reason contacts.LinkReason,
) error {
	// contactID here is externalID for channel lookup; we need the contact UUID.
	// Look up the contact by (channel, externalID) to get its UUID.
	var cID uuid.UUID
	row := tx.QueryRow(ctx, `
		SELECT contact_id FROM contact_channel_identity
		 WHERE channel = $1 AND external_id = $2
		 LIMIT 1
	`, channel, contactID)
	if err := row.Scan(&cID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no contact yet for this external_id; nothing to link
		}
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (contact_id) DO UPDATE SET identity_id = EXCLUDED.identity_id,
		                                       link_reason = EXCLUDED.link_reason
	`, tenantID, identityID, cID, string(reason))
	return err
}

func (s *IdentityStore) loadIdentity(
	ctx context.Context, tx pgx.Tx, tenantID, identityID uuid.UUID,
) (*contacts.Identity, error) {
	var createdAt time.Time
	var mergedInto *uuid.UUID
	row := tx.QueryRow(ctx, `
		SELECT created_at, merged_into_id FROM identity WHERE id = $1
	`, identityID)
	if err := row.Scan(&createdAt, &mergedInto); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, contacts.ErrNotFound
		}
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT id, contact_id, link_reason, created_at
		  FROM contact_identity_link
		 WHERE identity_id = $1
		 ORDER BY created_at ASC
	`, identityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []contacts.IdentityLink
	for rows.Next() {
		var l contacts.IdentityLink
		var reason string
		if err := rows.Scan(&l.ID, &l.ContactID, &reason, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.IdentityID = identityID
		l.Reason = contacts.LinkReason(reason)
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &contacts.Identity{
		ID:           identityID,
		TenantID:     tenantID,
		CreatedAt:    createdAt,
		MergedIntoID: mergedInto,
		Links:        links,
	}, nil
}

// mergeInTx re-points all contact_identity_link rows from sourceID to
// targetID and marks sourceID.merged_into_id = targetID. Uses SELECT …
// ORDER BY id FOR UPDATE so PostgreSQL acquires both row locks in a
// deterministic order, preventing deadlocks when two concurrent merges
// swap source/target.
func (s *IdentityStore) mergeInTx(
	ctx context.Context, tx pgx.Tx,
	tenantID, sourceID, targetID uuid.UUID,
) error {
	if _, err := tx.Exec(ctx, `
		SELECT id FROM identity WHERE id IN ($1, $2) ORDER BY id FOR UPDATE
	`, sourceID, targetID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE contact_identity_link
		   SET identity_id = $1
		 WHERE identity_id = $2
	`, targetID, sourceID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE identity SET merged_into_id = $1 WHERE id = $2
	`, targetID, sourceID); err != nil {
		return err
	}
	return nil
}
