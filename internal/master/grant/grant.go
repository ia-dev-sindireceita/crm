// Package grant implements the master courtesy-grant domain (SIN-62241 / F39).
//
// The package is hexagonal: this file and policy.go contain pure domain types
// with no I/O, ports.go declares the interfaces injected by adapters, and
// service.go orchestrates use-cases. Adapters live in sibling packages
// (memory, httpx, postgres, slack) and depend inward.
package grant

import (
	"errors"
	"strings"
	"time"
)

// Status is the lifecycle state of a courtesy grant request.
type Status string

const (
	// StatusGranted means the grant was within all caps and applied.
	StatusGranted Status = "granted"
	// StatusDeniedCapExceeded means at least one cap was breached and no
	// approval flow is configured (F6 not present); the request is rejected.
	StatusDeniedCapExceeded Status = "denied_cap_exceeded"
	// StatusPendingApproval means at least one cap was breached but an
	// approval flow (F6 4-eyes) is configured; the request is parked
	// awaiting a second master.
	StatusPendingApproval Status = "pending_approval"
	// StatusApproved means a previously pending grant has been ratified by
	// a second master and applied.
	StatusApproved Status = "approved"
	// StatusCancelled means a previously pending grant was rejected by the
	// ratifying master.
	StatusCancelled Status = "cancelled"
)

// Request is an inbound courtesy-grant intent before policy evaluation.
type Request struct {
	MasterID       string
	TenantID       string
	SubscriptionID string
	Amount         int64
	Reason         string
}

// Validate ensures the request is well-formed before policy evaluation.
// Boundary validation per the secure-by-default API rule.
func (r Request) Validate() error {
	if strings.TrimSpace(r.MasterID) == "" {
		return ErrInvalidMaster
	}
	if strings.TrimSpace(r.TenantID) == "" {
		return ErrInvalidTenant
	}
	if strings.TrimSpace(r.SubscriptionID) == "" {
		return ErrInvalidSubscription
	}
	if r.Amount <= 0 {
		return ErrInvalidAmount
	}
	if strings.TrimSpace(r.Reason) == "" {
		return ErrInvalidReason
	}
	return nil
}

// Grant is a persisted courtesy-grant decision.
type Grant struct {
	ID             string
	MasterID       string
	TenantID       string
	SubscriptionID string
	Amount         int64
	Reason         string
	Status         Status
	CreatedAt      time.Time
	// DecidedBy is the master id of the second master that ratified or
	// rejected a pending grant. Empty for non-pending statuses.
	DecidedBy string
	// DecidedAt is the time of the ratification decision. Zero for
	// non-pending statuses.
	DecidedAt time.Time
}

var (
	// ErrInvalidMaster is returned when the master id is missing.
	ErrInvalidMaster = errors.New("grant: master id required")
	// ErrInvalidTenant is returned when the tenant id is missing.
	ErrInvalidTenant = errors.New("grant: tenant id required")
	// ErrInvalidSubscription is returned when the subscription id is missing.
	ErrInvalidSubscription = errors.New("grant: subscription id required")
	// ErrInvalidAmount is returned when the amount is non-positive.
	ErrInvalidAmount = errors.New("grant: amount must be positive")
	// ErrInvalidReason is returned when the reason text is missing.
	ErrInvalidReason = errors.New("grant: reason required")
	// ErrRequiresApproval is returned to callers when the grant breached a
	// cap and either was denied (F6 absent) or parked pending approval.
	// Callers (HTTP adapter) must surface this as 403 "requires approval".
	ErrRequiresApproval = errors.New("grant: requires approval")
)
