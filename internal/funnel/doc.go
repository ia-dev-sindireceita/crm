// Package funnel holds the basic sales-funnel bounded context:
// stage definitions per tenant and an append-only ledger of
// per-conversation stage transitions.
//
// The package is the domain core: it imports neither database/sql nor
// pgx, neither net/http nor any vendor SDK. Storage lives behind
// StageRepository and TransitionRepository; event fan-out lives behind
// EventPublisher; the clock is injectable.
//
// The funnel is intentionally DDD-lite: it does NOT import
// internal/inbox. Conversations and users are referenced only by
// uuid.UUID so the two contexts can evolve independently.
//
// SIN-62792 (Fase 2 F2-08, child of SIN-62194). Drag-and-drop UI in
// F2-12 (SIN-62797); automatic transition rules land in Fase 4.
package funnel
