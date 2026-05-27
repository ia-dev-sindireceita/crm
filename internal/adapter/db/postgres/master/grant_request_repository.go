// Package master is the pgx-backed adapter for the master-side
// ports declared in internal/web/master. SIN-63605 introduces the
// 4-eyes approval surface (master_grant_request, migration 0097);
// this package owns the repository for that table.
//
// Every method runs under postgres.WithMasterOps so the
// master_ops_audit trigger from migration 0002 records the operator
// for every insert / update / delete. The pool MUST be the
// app_master_ops pool — connecting as a different role makes the
// trigger abort the transaction, which is the load-bearing safety
// property.
package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/wallet"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// Compile-time assertions that GrantRequestStore satisfies every
// sub-port in masterweb.GrantRequestPort. A drifted port signature
// fails the build here before reaching the caller.
var (
	_ masterweb.GrantRequestCreator  = (*GrantRequestStore)(nil)
	_ masterweb.GrantRequestLister   = (*GrantRequestStore)(nil)
	_ masterweb.GrantRequestApprover = (*GrantRequestStore)(nil)
	_ masterweb.GrantRequestRejecter = (*GrantRequestStore)(nil)
)

// GrantRequestStore is the pgx-backed repository for master_grant_request.
// Construct with NewGrantRequestStore; the pool MUST be the master_ops
// pool. actorID is the master user currently driving the console — it
// is threaded into every WithMasterOps call so the audit trigger writes
// an attributable row.
//
// Notes on the approve path:
//
//   - The TX SELECTs created_by_user_id first; if it matches actor we
//     return ErrGrantRequestApproverIsCreator before any write so the
//     CHECK in 0097 never has to fire (defence in depth).
//   - The UPDATE has a state='awaiting_approval' guard. When another
//     master beats us to the decision, rowsAffected==0 and we return
//     ErrGrantRequestAlreadyDecided.
//   - The master_grant INSERT lives in the same TX as the UPDATE — an
//     approved request must always have its promoted grant row.
//   - Now is the clock injected for tests; production passes time.Now.
type GrantRequestStore struct {
	pool    postgresadapter.TxBeginner
	actorID uuid.UUID
	now     func() time.Time
}

// NewGrantRequestStore validates inputs and returns a Store. Tests can
// override the clock by passing WithClock; production callers use the
// no-option form and get time.Now.
func NewGrantRequestStore(pool *pgxpool.Pool, actorID uuid.UUID, opts ...Option) (*GrantRequestStore, error) {
	if pool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	s := &GrantRequestStore{pool: pool, actorID: actorID, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Option is the constructor-option seam used by tests for clock
// injection; production callers do not need it.
type Option func(*GrantRequestStore)

// WithClock pins the clock used for decided_at / created_at fallback.
// Production passes time.Now (the default); tests freeze a value to
// assert on the exact stored timestamp.
func WithClock(now func() time.Time) Option {
	return func(s *GrantRequestStore) {
		if now != nil {
			s.now = now
		}
	}
}

// CreateGrantRequest INSERTs a new master_grant_request row with a
// freshly-minted ULID external_id, payload encoded the same way as
// master_grant (jsonb), state='awaiting_approval'.
func (s *GrantRequestStore) CreateGrantRequest(ctx context.Context, in masterweb.CreateGrantRequestInput) (masterweb.GrantRequest, error) {
	if in.ActorUserID == uuid.Nil {
		return masterweb.GrantRequest{}, fmt.Errorf("master/postgres: create grant request: %w", postgresadapter.ErrZeroActor)
	}
	payload, err := encodePayload(in.Kind, in.PeriodDays, in.Amount)
	if err != nil {
		return masterweb.GrantRequest{}, err
	}
	id := uuid.New()
	externalID := wallet.NewULID()
	createdAt := s.now().UTC()
	err = postgresadapter.WithMasterOps(ctx, s.pool, in.ActorUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO master_grant_request
			  (id, external_id, tenant_id, kind, payload, reason,
			   created_by_user_id, state, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'awaiting_approval',$8)`,
			id, externalID, in.TenantID, string(in.Kind), payload,
			in.Reason, in.ActorUserID, createdAt,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: insert master_grant_request: %w", err)
		}
		return nil
	})
	if err != nil {
		return masterweb.GrantRequest{}, err
	}
	return masterweb.GrantRequest{
		ID:          id,
		ExternalID:  externalID,
		TenantID:    in.TenantID,
		Kind:        in.Kind,
		PeriodDays:  in.PeriodDays,
		Amount:      in.Amount,
		Reason:      in.Reason,
		CreatedByID: in.ActorUserID,
		State:       masterweb.GrantRequestStateAwaiting,
		CreatedAt:   createdAt,
	}, nil
}

// ListAwaitingRequests returns every awaiting_approval request ordered
// by created_at DESC.
func (s *GrantRequestStore) ListAwaitingRequests(ctx context.Context) ([]masterweb.GrantRequest, error) {
	var out []masterweb.GrantRequest
	err := postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, external_id, tenant_id, kind, payload, reason,
			       created_by_user_id, requires_second_approver_id,
			       state, decided_at, created_at
			  FROM master_grant_request
			 WHERE state = 'awaiting_approval'
			 ORDER BY created_at DESC`,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: query awaiting requests: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			req, err := scanRequestRow(rows)
			if err != nil {
				return err
			}
			out = append(out, req)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []masterweb.GrantRequest{}
	}
	return out, nil
}

// GetGrantRequest returns the request with id. ErrGrantRequestNotFound
// is the sentinel for "no row".
func (s *GrantRequestStore) GetGrantRequest(ctx context.Context, id uuid.UUID) (masterweb.GrantRequest, error) {
	var req masterweb.GrantRequest
	err := postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		got, err := scanRequest(tx.QueryRow(ctx, selectRequestByID, id))
		if err != nil {
			return err
		}
		req = got
		return nil
	})
	if err != nil {
		return masterweb.GrantRequest{}, err
	}
	return req, nil
}

// ApproveGrantRequest runs the approve flow in a single TX:
//
//  1. SELECT the request for state + created_by guard.
//  2. Reject when actor == requester (ErrGrantRequestApproverIsCreator).
//  3. UPDATE … WHERE state='awaiting_approval'. When rowsAffected==0
//     return ErrGrantRequestAlreadyDecided (rolls back implicitly).
//  4. INSERT the master_grant row. Same TX so the audit trail shows
//     both the request update and the grant insert under the same
//     actor.
func (s *GrantRequestStore) ApproveGrantRequest(ctx context.Context, in masterweb.DecideGrantRequestInput) (masterweb.GrantRow, error) {
	if in.ActorUserID == uuid.Nil {
		return masterweb.GrantRow{}, fmt.Errorf("master/postgres: approve grant request: %w", postgresadapter.ErrZeroActor)
	}
	var grant masterweb.GrantRow
	err := postgresadapter.WithMasterOps(ctx, s.pool, in.ActorUserID, func(tx pgx.Tx) error {
		req, err := scanRequest(tx.QueryRow(ctx, selectRequestByID, in.RequestID))
		if err != nil {
			return err
		}
		if req.State != masterweb.GrantRequestStateAwaiting {
			return masterweb.ErrGrantRequestAlreadyDecided
		}
		if req.CreatedByID == in.ActorUserID {
			return masterweb.ErrGrantRequestApproverIsCreator
		}
		now := s.now().UTC()
		ct, err := tx.Exec(ctx, `
			UPDATE master_grant_request
			   SET state                       = 'approved',
			       requires_second_approver_id = $1,
			       decided_at                  = $2
			 WHERE id    = $3
			   AND state = 'awaiting_approval'`,
			in.ActorUserID, now, in.RequestID,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: update master_grant_request approve: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return masterweb.ErrGrantRequestAlreadyDecided
		}
		payload, perr := encodePayload(req.Kind, req.PeriodDays, req.Amount)
		if perr != nil {
			return perr
		}
		grantID := uuid.New()
		grantExternal := wallet.NewULID()
		if _, err := tx.Exec(ctx, `
			INSERT INTO master_grant
			  (id, external_id, tenant_id, kind, payload, reason,
			   created_by_user_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			grantID, grantExternal, req.TenantID, string(req.Kind),
			payload, req.Reason, req.CreatedByID, now,
		); err != nil {
			return fmt.Errorf("master/postgres: insert master_grant (4-eyes approved): %w", err)
		}
		grant = masterweb.GrantRow{
			ID:          grantID,
			ExternalID:  grantExternal,
			TenantID:    req.TenantID,
			Kind:        req.Kind,
			PeriodDays:  req.PeriodDays,
			Amount:      req.Amount,
			Reason:      req.Reason,
			CreatedByID: req.CreatedByID,
			CreatedAt:   now,
		}
		return nil
	})
	if err != nil {
		return masterweb.GrantRow{}, err
	}
	return grant, nil
}

// RejectGrantRequest transitions the request to rejected. The DB
// CHECK forbids requires_second_approver_id == created_by_user_id, so
// we run the actor==requester guard before issuing the UPDATE for the
// same reason as Approve (clean 422 instead of a 500 wrapping
// SQLSTATE 23514).
func (s *GrantRequestStore) RejectGrantRequest(ctx context.Context, in masterweb.DecideGrantRequestInput) error {
	if in.ActorUserID == uuid.Nil {
		return fmt.Errorf("master/postgres: reject grant request: %w", postgresadapter.ErrZeroActor)
	}
	return postgresadapter.WithMasterOps(ctx, s.pool, in.ActorUserID, func(tx pgx.Tx) error {
		req, err := scanRequest(tx.QueryRow(ctx, selectRequestByID, in.RequestID))
		if err != nil {
			return err
		}
		if req.State != masterweb.GrantRequestStateAwaiting {
			return masterweb.ErrGrantRequestAlreadyDecided
		}
		if req.CreatedByID == in.ActorUserID {
			return masterweb.ErrGrantRequestApproverIsCreator
		}
		now := s.now().UTC()
		ct, err := tx.Exec(ctx, `
			UPDATE master_grant_request
			   SET state                       = 'rejected',
			       requires_second_approver_id = $1,
			       decided_at                  = $2
			 WHERE id    = $3
			   AND state = 'awaiting_approval'`,
			in.ActorUserID, now, in.RequestID,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: update master_grant_request reject: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return masterweb.ErrGrantRequestAlreadyDecided
		}
		return nil
	})
}

// --- queries ---------------------------------------------------------

const selectRequestByID = `
	SELECT id, external_id, tenant_id, kind, payload, reason,
	       created_by_user_id, requires_second_approver_id,
	       state, decided_at, created_at
	  FROM master_grant_request
	 WHERE id = $1
`

func scanRequest(row pgx.Row) (masterweb.GrantRequest, error) {
	return decodeRequest(row.Scan)
}

func scanRequestRow(rows pgx.Rows) (masterweb.GrantRequest, error) {
	return decodeRequest(rows.Scan)
}

func decodeRequest(scan func(...any) error) (masterweb.GrantRequest, error) {
	var (
		id, tenantID, createdByID uuid.UUID
		externalID, kind, reason  string
		payloadRaw                []byte
		secondApprover            *uuid.UUID
		state                     string
		decidedAt                 *time.Time
		createdAt                 time.Time
	)
	if err := scan(
		&id, &externalID, &tenantID, &kind, &payloadRaw, &reason,
		&createdByID, &secondApprover, &state, &decidedAt, &createdAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return masterweb.GrantRequest{}, masterweb.ErrGrantRequestNotFound
		}
		return masterweb.GrantRequest{}, fmt.Errorf("master/postgres: scan master_grant_request: %w", err)
	}
	periodDays, amount, err := decodePayload(masterweb.GrantKind(kind), payloadRaw)
	if err != nil {
		return masterweb.GrantRequest{}, err
	}
	out := masterweb.GrantRequest{
		ID:          id,
		ExternalID:  externalID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKind(kind),
		PeriodDays:  periodDays,
		Amount:      amount,
		Reason:      reason,
		CreatedByID: createdByID,
		State:       masterweb.GrantRequestState(state),
		CreatedAt:   createdAt,
	}
	if secondApprover != nil {
		out.SecondApproverID = *secondApprover
	}
	if decidedAt != nil {
		out.DecidedAt = *decidedAt
	}
	return out, nil
}

// encodePayload marshals the kind-specific projection used by master_grant_request.payload (jsonb).
// The shape mirrors master_grant.payload (wallet adapter): {"period_days": N}
// for free_subscription_period; {"tokens": N} for extra_tokens.
func encodePayload(kind masterweb.GrantKind, periodDays int, amount int64) ([]byte, error) {
	var payload map[string]any
	switch kind {
	case masterweb.GrantKindFreeSubscriptionPeriod:
		payload = map[string]any{"period_days": periodDays}
	case masterweb.GrantKindExtraTokens:
		payload = map[string]any{"tokens": amount}
	default:
		return nil, fmt.Errorf("master/postgres: unknown grant kind %q", string(kind))
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("master/postgres: marshal payload: %w", err)
	}
	return out, nil
}

func decodePayload(kind masterweb.GrantKind, raw []byte) (int, int64, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, 0, fmt.Errorf("master/postgres: unmarshal payload: %w", err)
	}
	switch kind {
	case masterweb.GrantKindFreeSubscriptionPeriod:
		days, _ := numberToInt(payload["period_days"])
		return days, 0, nil
	case masterweb.GrantKindExtraTokens:
		_, tokens := numberToInt(payload["tokens"])
		return 0, tokens, nil
	}
	return 0, 0, nil
}

// numberToInt accepts either a json.Number-ish float64 (encoding/json
// default) or a string and returns its int / int64 projection. Returns
// (0,0) when the value is missing or not numeric.
func numberToInt(v any) (int, int64) {
	switch n := v.(type) {
	case float64:
		return int(n), int64(n)
	case int:
		return n, int64(n)
	case int64:
		return int(n), n
	}
	return 0, 0
}
