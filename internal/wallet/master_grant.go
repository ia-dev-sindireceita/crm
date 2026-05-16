package wallet

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// MasterGrantKind enumerates the two flavours of master-issued grants
// (ADR-0098 §D1, migration 0097).
type MasterGrantKind string

const (
	// KindFreeSubscriptionPeriod credits the tenant with a billing
	// period free of charge (C8/subscription writer handles the
	// downstream ledger entry).
	KindFreeSubscriptionPeriod MasterGrantKind = "free_subscription_period"

	// KindExtraTokens credits the tenant's token wallet directly.
	// The amount is written to token_ledger with source='master_grant'.
	// Grants of this kind are subject to the per-grant cap that the
	// courtesy_grant adapter enforces (SIN-62241 / ADR-0093).
	KindExtraTokens MasterGrantKind = "extra_tokens"
)

// MasterGrant is the domain representation of a master-issued grant.
// It maps to the master_grant table (migration 0097, ADR-0098 §D1).
//
// The primary key in the database is an internal UUID (id); ExternalID
// is a ULID generated in Go before insertion (AC §5 — do not use
// gen_random_uuid on the Postgres side).
//
// State machine (ADR-0098 §D1, §D4):
//
//	created → revoked  (Revoke — only while ConsumedAt == nil)
//	created → consumed (Consume — only while RevokedAt == nil)
//
// Both transitions are terminal; a revoked grant cannot be consumed
// and vice versa.
type MasterGrant struct {
	id              uuid.UUID
	externalID      string
	tenantID        uuid.UUID
	kind            MasterGrantKind
	payload         map[string]any
	reason          string
	createdByUserID uuid.UUID
	createdAt       time.Time
	consumedAt      *time.Time
	consumedRef     string
	revokedAt       *time.Time
	revokedByUserID *uuid.UUID
	revokeReason    string
}

// --- constructors ----------------------------------------------------

// NewMasterGrant creates a fresh MasterGrant. ExternalID is generated
// by NewULID(); the caller supplies all other required fields.
// Reason must be at least 10 characters (mirrors the DB CHECK).
func NewMasterGrant(
	tenantID, createdByUserID uuid.UUID,
	kind MasterGrantKind,
	payload map[string]any,
	reason string,
	now time.Time,
) (*MasterGrant, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if createdByUserID == uuid.Nil {
		return nil, ErrZeroActor
	}
	if kind != KindFreeSubscriptionPeriod && kind != KindExtraTokens {
		return nil, ErrInvalidGrantKind
	}
	if len(reason) < 10 {
		return nil, ErrGrantReasonTooShort
	}
	return &MasterGrant{
		id:              uuid.New(),
		externalID:      NewULID(),
		tenantID:        tenantID,
		kind:            kind,
		payload:         payload,
		reason:          reason,
		createdByUserID: createdByUserID,
		createdAt:       now,
	}, nil
}

// HydrateMasterGrant rebuilds a MasterGrant from durable state.
// Only adapters should call this; invariants are not re-validated
// because the database already enforces them.
func HydrateMasterGrant(
	id uuid.UUID,
	externalID string,
	tenantID, createdByUserID uuid.UUID,
	kind MasterGrantKind,
	payload map[string]any,
	reason string,
	createdAt time.Time,
	consumedAt *time.Time,
	consumedRef string,
	revokedAt *time.Time,
	revokedByUserID *uuid.UUID,
	revokeReason string,
) *MasterGrant {
	return &MasterGrant{
		id:              id,
		externalID:      externalID,
		tenantID:        tenantID,
		kind:            kind,
		payload:         payload,
		reason:          reason,
		createdByUserID: createdByUserID,
		createdAt:       createdAt,
		consumedAt:      consumedAt,
		consumedRef:     consumedRef,
		revokedAt:       revokedAt,
		revokedByUserID: revokedByUserID,
		revokeReason:    revokeReason,
	}
}

// --- accessors -------------------------------------------------------

func (g *MasterGrant) ID() uuid.UUID               { return g.id }
func (g *MasterGrant) ExternalID() string          { return g.externalID }
func (g *MasterGrant) TenantID() uuid.UUID         { return g.tenantID }
func (g *MasterGrant) Kind() MasterGrantKind       { return g.kind }
func (g *MasterGrant) Payload() map[string]any     { return g.payload }
func (g *MasterGrant) Reason() string              { return g.reason }
func (g *MasterGrant) CreatedByUserID() uuid.UUID  { return g.createdByUserID }
func (g *MasterGrant) CreatedAt() time.Time        { return g.createdAt }
func (g *MasterGrant) ConsumedAt() *time.Time      { return g.consumedAt }
func (g *MasterGrant) ConsumedRef() string         { return g.consumedRef }
func (g *MasterGrant) RevokedAt() *time.Time       { return g.revokedAt }
func (g *MasterGrant) RevokedByUserID() *uuid.UUID { return g.revokedByUserID }
func (g *MasterGrant) RevokeReason() string        { return g.revokeReason }

// IsRevoked reports whether the grant has been revoked.
func (g *MasterGrant) IsRevoked() bool { return g.revokedAt != nil }

// IsConsumed reports whether the grant has been consumed.
func (g *MasterGrant) IsConsumed() bool { return g.consumedAt != nil }

// --- transitions -----------------------------------------------------

// Revoke marks the grant as revoked. Returns ErrGrantAlreadyConsumed
// if the grant has already been consumed, or ErrGrantAlreadyRevoked if
// it has already been revoked. The revokeReason must be ≥10 chars.
func (g *MasterGrant) Revoke(revokedByUserID uuid.UUID, revokeReason string, now time.Time) error {
	if g.consumedAt != nil {
		return ErrGrantAlreadyConsumed
	}
	if g.revokedAt != nil {
		return ErrGrantAlreadyRevoked
	}
	if revokedByUserID == uuid.Nil {
		return ErrZeroActor
	}
	if len(revokeReason) < 10 {
		return ErrGrantReasonTooShort
	}
	g.revokedAt = &now
	g.revokedByUserID = &revokedByUserID
	g.revokeReason = revokeReason
	return nil
}

// Consume marks the grant as consumed, recording the ref to the
// downstream artifact (e.g. ledger entry ID or subscription external
// id). Returns ErrGrantAlreadyRevoked if revoked, ErrGrantAlreadyConsumed
// if already consumed.
func (g *MasterGrant) Consume(ref string, now time.Time) error {
	if g.revokedAt != nil {
		return ErrGrantAlreadyRevoked
	}
	if g.consumedAt != nil {
		return ErrGrantAlreadyConsumed
	}
	g.consumedAt = &now
	g.consumedRef = ref
	return nil
}

// --- errors ----------------------------------------------------------

var (
	// ErrZeroActor is returned when a zero UUID is passed as a user id.
	ErrZeroActor = errors.New("wallet: actor user id must not be uuid.Nil")

	// ErrInvalidGrantKind is returned when an unrecognised MasterGrantKind
	// is supplied to NewMasterGrant.
	ErrInvalidGrantKind = errors.New("wallet: unknown master grant kind")

	// ErrGrantReasonTooShort is returned when reason/revokeReason is
	// shorter than 10 characters (mirrors the DB CHECK).
	ErrGrantReasonTooShort = errors.New("wallet: reason must be at least 10 characters")

	// ErrGrantAlreadyConsumed is returned by Revoke when the grant has
	// already been consumed (ADR-0098 §D4 terminal state).
	ErrGrantAlreadyConsumed = errors.New("wallet: master grant already consumed")

	// ErrGrantAlreadyRevoked is returned by Consume when the grant has
	// already been revoked (ADR-0098 §D4 terminal state).
	ErrGrantAlreadyRevoked = errors.New("wallet: master grant already revoked")
)
