package master

// SIN-63605 / Fase 2.5 follow-up — 4-eyes approval flow for grants
// above the per-grant or per-tenant cap. A grant that exceeds the cap
// (ErrPerGrantCapExceeded / ErrPerTenantWindowCapExceeded in
// grants.go) is NOT inserted directly into master_grant. Instead it
// lands in master_grant_request (migration 0097) with
// state=awaiting_approval, no second-approver, no decision timestamp.
// A second master then either Approves (writes the master_grant row in
// the same TX) or Rejects. The schema CHECK + the adapter SELECT both
// enforce that the approver is a different user than the requester.
//
// Ports declared here are intentionally narrow: each verb is its own
// interface so the handler tests can stub a single capability without
// dragging in the rest. cmd/server composes the adapter into a single
// struct that embeds them all (GrantRequestPort below) and passes it
// through master.Deps.
//
// Hexagonal boundary: this file imports nothing from database/sql,
// pgx, or net/http — those live in the adapter and handler files
// respectively.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// GrantRequestState mirrors the DB state machine declared in
// migration 0097's master_grant_request_state_consistency CHECK.
// The string values are part of the contract: the adapter writes
// them to the DB and the handler templates compare them.
type GrantRequestState string

const (
	// GrantRequestStateAwaiting is the initial state after Create.
	// requires_second_approver_id and decided_at are both NULL.
	GrantRequestStateAwaiting GrantRequestState = "awaiting_approval"
	// GrantRequestStateApproved is set by Approve. The second approver
	// and decision timestamp are both populated; a master_grant row
	// has been written in the same TX (ADR-0098 §D5).
	GrantRequestStateApproved GrantRequestState = "approved"
	// GrantRequestStateRejected is set by Reject. The second approver
	// (the rejecter) and decision timestamp are both populated; no
	// master_grant row is written.
	GrantRequestStateRejected GrantRequestState = "rejected"
)

// GrantRequest is the projection rendered on the awaiting-approval list
// and the detail page. Fields mirror the master_grant_request schema;
// the PeriodDays / Amount split mirrors the same projection on GrantRow
// (grants.go) so the same kind switch works in templates and the
// promotion-to-master_grant path can lift the payload verbatim.
type GrantRequest struct {
	ID               uuid.UUID
	ExternalID       string
	TenantID         uuid.UUID
	Kind             GrantKind
	PeriodDays       int
	Amount           int64
	Reason           string
	CreatedByID      uuid.UUID
	SecondApproverID uuid.UUID
	State            GrantRequestState
	DecidedAt        time.Time
	CreatedAt        time.Time
}

// CapEquivalence returns the tokens-equivalent value of the request's
// payload using the same arithmetic the grant cap uses. Exposed so
// templates can show "equivalent to N tokens" in the review UI.
func (r GrantRequest) CapEquivalence() int64 {
	return CapEquivalence(r.Kind, r.Amount, r.PeriodDays)
}

// CreateGrantRequestInput is the form-derived payload for POST
// /master/tenants/{id}/grants/requests. The shape mirrors
// IssueGrantInput (grants.go) so a failed cap check on the regular
// grant form can re-POST the same body to the request endpoint.
type CreateGrantRequestInput struct {
	ActorUserID uuid.UUID
	TenantID    uuid.UUID
	Kind        GrantKind
	PeriodDays  int   // KindFreeSubscriptionPeriod only
	Amount      int64 // KindExtraTokens only
	Reason      string
}

// DecideGrantRequestInput is the body of the approve / reject POSTs.
// RequestID is path-derived; ActorUserID is the resolved Principal.
// Reason is reserved for future use (the schema does not yet store a
// decision reason; the request reason and the audit row carry the
// motivation today).
type DecideGrantRequestInput struct {
	ActorUserID uuid.UUID
	RequestID   uuid.UUID
	Reason      string
}

// GrantRequestCreator is the write-side port for POST
// /master/tenants/{id}/grants/requests. The adapter generates the ULID
// external_id and inserts under WithMasterOps so the audit trigger
// records the actor.
type GrantRequestCreator interface {
	CreateGrantRequest(ctx context.Context, in CreateGrantRequestInput) (GrantRequest, error)
}

// GrantRequestLister is the read-side port. ListAwaitingRequests powers
// the GET /master/grants/requests inbox; GetGrantRequest powers the
// detail page and the approve/reject confirmation step.
type GrantRequestLister interface {
	ListAwaitingRequests(ctx context.Context) ([]GrantRequest, error)
	GetGrantRequest(ctx context.Context, id uuid.UUID) (GrantRequest, error)
}

// GrantRequestApprover is the write-side port for POST
// /master/grants/requests/{id}/approve. The adapter MUST run the
// state-transition UPDATE and the master_grant INSERT in the same TX
// so an approved request always has its promoted grant. ErrGrantRequest
// ApproverIsCreator and ErrGrantRequestAlreadyDecided are the two
// domain-level rejections; everything else is wrapped with
// fmt.Errorf("%w", …) so callers can match with errors.Is.
type GrantRequestApprover interface {
	ApproveGrantRequest(ctx context.Context, in DecideGrantRequestInput) (GrantRow, error)
}

// GrantRequestRejecter is the write-side port for POST
// /master/grants/requests/{id}/reject. Idempotent at the DB layer
// (UPDATE … WHERE state='awaiting_approval'); ErrGrantRequest
// ApproverIsCreator and ErrGrantRequestAlreadyDecided mirror the
// approver path so the handler maps each to the same status code on
// both verbs.
type GrantRequestRejecter interface {
	RejectGrantRequest(ctx context.Context, in DecideGrantRequestInput) error
}

// GrantRequestPort is the union of the four sub-ports the C10
// 4-eyes surface needs. cmd/server wires this with the postgres
// adapter; handler tests stub one or more sub-ports as needed.
type GrantRequestPort interface {
	GrantRequestCreator
	GrantRequestLister
	GrantRequestApprover
	GrantRequestRejecter
}

// Domain-level errors returned by the request ports. They map to HTTP
// status codes in grant_requests_handlers.go:
//
//   - ErrGrantRequestNotFound          → 404
//   - ErrGrantRequestAlreadyDecided    → 409
//   - ErrGrantRequestApproverIsCreator → 422
var (
	// ErrGrantRequestNotFound is returned by Get/Approve/Reject when
	// the request id does not exist. Handler → 404.
	ErrGrantRequestNotFound = errors.New("web/master: grant request not found")

	// ErrGrantRequestAlreadyDecided signals a race or a duplicate
	// click — the request was already approved or rejected by another
	// master between the GET that rendered the form and this POST.
	// Handler → 409 so the UI can refresh.
	ErrGrantRequestAlreadyDecided = errors.New("web/master: grant request already decided")

	// ErrGrantRequestApproverIsCreator enforces the 4-eyes invariant
	// at the use-case layer (defence in depth on top of the schema
	// CHECK in 0097). Handler → 422 so the operator sees a clear
	// "approver cannot be the requester" message instead of a 500.
	ErrGrantRequestApproverIsCreator = errors.New("web/master: grant request approver cannot be the requester")
)
