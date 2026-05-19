// Package management orchestrates the tenant-facing custom-domain
// lifecycle: list, enroll, verify, pause/resume, delete.
//
// Sits on top of the existing F45 building blocks:
//
//   - enrollment.UseCase enforces the per-tenant quota gate
//   - slugreservation.Service holds the 12-month reservation lock on
//     teardown
//   - validation (when SIN-62242 lands) enforces FQDN + DNS rules
//
// The use-case stays HTTP-agnostic. The HTTP boundary
// (internal/adapter/transport/http/customdomain) translates Result and
// errors to status codes + PT-BR copy.
package management

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Domain is the tenant-facing snapshot of one tenant_custom_domains row.
// All optional timestamps are pointers so the templates can branch on
// nil vs set without sentinel values. The HTTP layer converts to view
// models for rendering.
//
// FailedAt + FailureReason capture the terminal "verification gave up"
// state the customdomain-verifier worker writes after exhausting its
// in-memory attempt cap ([SIN-63080]). A non-nil FailedAt is a one-way
// transition — the row drops out of ListPendingVerification and the UI
// renders it as StatusFailed. FailureReason carries a short controlled-
// vocabulary string (cap_exceeded / token_mismatch_cap / resolver_cap)
// so support can answer "why did this stop being polled?" without
// joining the audit log.
type Domain struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	Host               string
	VerificationToken  string
	VerifiedAt         *time.Time
	VerifiedWithDNSSEC bool
	TLSPausedAt        *time.Time
	DeletedAt          *time.Time
	FailedAt           *time.Time
	FailureReason      string
	DNSResolutionLogID *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Status is the four-state UI badge derived from the timestamps. Errors
// in the most recent verification attempt produce StatusError carrying
// the reason code; the template paints the badge red and shows the
// PT-BR string in a tooltip.
type Status int

const (
	StatusUnknown  Status = iota
	StatusPending         // verified_at IS NULL, no error
	StatusVerified        // verified_at IS NOT NULL, tls_paused_at IS NULL
	StatusPaused          // tls_paused_at IS NOT NULL
	StatusError           // last verify attempt failed (transient)
	StatusFailed          // failed_at IS NOT NULL — verifier worker gave up (terminal)
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusVerified:
		return "verified"
	case StatusPaused:
		return "paused"
	case StatusError:
		return "error"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// StatusOf maps a Domain plus optional last-verify error to its UI
// state. Verified-and-paused renders as Paused (the operational
// concern dominates the visual cue). StatusFailed (verifier worker
// cap exhausted) outranks everything except Paused — a paused row
// that also has failed_at set is still operationally a paused row.
func StatusOf(d Domain, lastVerifyErr error) Status {
	if d.TLSPausedAt != nil {
		return StatusPaused
	}
	if d.VerifiedAt != nil {
		return StatusVerified
	}
	if d.FailedAt != nil {
		return StatusFailed
	}
	if lastVerifyErr != nil {
		return StatusError
	}
	return StatusPending
}

// Reason codes the verify path returns to the HTTP boundary. The
// boundary maps to PT-BR strings in copy_pt_br.go.
type Reason int

const (
	ReasonNone Reason = iota
	ReasonInvalidHost
	ReasonPrivateIP
	ReasonTokenMismatch
	ReasonDNSResolutionFailed
	ReasonRateLimited
	ReasonSlugReserved
	ReasonNotFound
	ReasonAlreadyVerified
	ReasonForbidden
	ReasonInternal
)

func (r Reason) String() string {
	switch r {
	case ReasonInvalidHost:
		return "invalid_host"
	case ReasonPrivateIP:
		return "private_ip"
	case ReasonTokenMismatch:
		return "token_mismatch"
	case ReasonDNSResolutionFailed:
		return "dns_resolution_failed"
	case ReasonRateLimited:
		return "rate_limited"
	case ReasonSlugReserved:
		return "slug_reserved"
	case ReasonNotFound:
		return "not_found"
	case ReasonAlreadyVerified:
		return "already_verified"
	case ReasonForbidden:
		return "forbidden"
	case ReasonInternal:
		return "internal"
	default:
		return "none"
	}
}

// VerifyOutcome is the structured return of a verify attempt. Verified
// is the post-verify domain snapshot when the attempt succeeds; on
// failure Reason is set and the boundary renders the PT-BR error.
type VerifyOutcome struct {
	Domain   Domain
	Verified bool
	Reason   Reason
	Err      error
}

// EnrollResult carries the freshly-inserted Domain plus the TXT
// instructions the wizard's step 2 displays.
type EnrollResult struct {
	Domain        Domain
	TXTRecord     string // "_crm-verify.<host>"
	TXTValue      string // "crm-verify=<token>"
	Reason        Reason
	RetryAfter    time.Duration // populated for ReasonRateLimited
	ReservedUntil *time.Time    // populated for ReasonSlugReserved
}

// Sentinel errors. The HTTP boundary uses errors.Is to map to status
// codes; the use-case never produces a 4xx/5xx itself.
var (
	ErrInvalidHost     = errors.New("management: invalid host")
	ErrPrivateIP       = errors.New("management: host resolves to a private IP")
	ErrTokenMismatch   = errors.New("management: TXT record missing or wrong value")
	ErrTenantMismatch  = errors.New("management: domain belongs to a different tenant")
	ErrAlreadyVerified = errors.New("management: domain already verified")
	ErrSlugReserved    = errors.New("management: slug is reserved")
)
