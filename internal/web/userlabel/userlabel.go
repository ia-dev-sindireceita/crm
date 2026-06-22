// Package userlabel resolves the logged-in user's id to the display
// label shown in the app-shell top-bar account button.
//
// It exists so the four authed web surfaces (contacts, dashboard, funnel,
// inbox) share ONE resolver instead of four copies of a uuid-prefix stub
// that rendered the raw id prefix (e.g. "00000000" for the seed
// atendente) instead of the user's name (SIN-65578).
//
// The label itself is derived from the email local-part by the Postgres
// adapter behind the Directory port (the users table carries no name
// column — migration 0005), exactly like the inbox assignment dropdown:
// the same user must render the same label everywhere.
package userlabel

import (
	"context"

	"github.com/google/uuid"
)

// Fallback is the label rendered whenever a real label cannot be
// resolved: the resolver is unwired, the id is nil/unknown, or the lookup
// errors. It is never the raw uuid.
const Fallback = "Conta"

// Directory resolves a set of tenant user ids to display labels. It is a
// structural mirror of the inbox domain's UserDirectory read port;
// declaring it here keeps internal/web/* off the inbox domain root
// (forbidwebboundary, SIN-62735) — the composition root satisfies it with
// the Postgres adapter (postgres/inbox.NewUserDirectory).
//
// Implementations MUST be tenant-scoped: a label lookup must never cross
// tenants. Ids with no matching user are simply absent from the returned
// map.
type Directory interface {
	LabelsByID(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Resolve returns the top-bar display label for userID under tenantID.
//
// Fallback chain (never the raw uuid):
//   - userID == uuid.Nil  → Fallback ("Conta")
//   - dir == nil          → Fallback (resolver unwired / smoke boot)
//   - lookup error        → Fallback (degrade, never 500 the shell)
//   - id absent / empty   → Fallback
//   - otherwise           → the resolved label (e.g. "agent")
//
// The single-id lookup is one extra query per page render; the port is
// already batch-shaped so it stays a single round-trip.
func Resolve(ctx context.Context, dir Directory, tenantID, userID uuid.UUID) string {
	if userID == uuid.Nil || dir == nil {
		return Fallback
	}
	labels, err := dir.LabelsByID(ctx, tenantID, []uuid.UUID{userID})
	if err != nil {
		return Fallback
	}
	if label, ok := labels[userID]; ok && label != "" {
		return label
	}
	return Fallback
}
