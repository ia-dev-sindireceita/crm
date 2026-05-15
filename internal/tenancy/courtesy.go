package tenancy

import (
	"context"

	"github.com/google/uuid"
)

// CourtesyGrantIssuer is the consumer-side port the tenant-create
// flow uses to bootstrap a fresh tenant's wallet (SIN-62730, ADR
// 0093). The flow MUST call IssueCourtesyGrant after the Tenant row
// is persisted; the implementation is idempotent so a retried
// create-tenant is safe.
//
// The interface is declared here — not in internal/wallet — so the
// tenant-create use-case (which lives next to the Tenant aggregate)
// depends on a port owned by its own package and never imports the
// wallet adapter. The wire-up at cmd/server adapts
// wallet/usecase.IssueCourtesyGrantService to this interface.
//
// Implementations MUST:
//
//   - Be safe to call concurrently for the same tenantID; collisions
//     resolve to "first wins, others are silent no-op".
//   - Surface the configured "disabled" mode as a sentinel the caller
//     can treat as a soft skip rather than aborting tenant creation.
//   - Persist their own audit trail (master_ops_audit covers the
//     wallet bootstrap; the tenancy flow does not need to mirror it).
type CourtesyGrantIssuer interface {
	IssueCourtesyGrant(ctx context.Context, tenantID uuid.UUID) error
}
