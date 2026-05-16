package main

// SIN-62862 wiring — HTMX funnel board UI (SIN-62797 / Fase 2 F2-12).
//
// buildWebFunnelHandler assembles the four dependencies the funnel
// handler needs:
//
//   - Mover           = funnel.Service (SIN-62792 / F2-08), wrapping the
//                       pgx Store as both StageRepository and
//                       TransitionRepository, with a slog-backed
//                       EventPublisher (placeholder until SIN-62194's
//                       message bus wiring lands — the transition row in
//                       Postgres is the source of truth).
//   - Board           = pgx funnel.Store.Board (one-round-trip projection).
//   - StageResolver   = same Store.FindByKey.
//   - FunnelHistory   = same Store.ListForConversation.
//   - AssignmentHistory = a thin adapter that calls inbox.Store.ListHistory
//                         and maps *inbox.Assignment → webfunnel.AssignmentEntry.
//                         This mapping lives in cmd/server (the composition
//                         root) to keep internal/web/funnel inside the
//                         forbidwebboundary lens (SIN-62735 / SIN-62862 AC).
//
// Returns (nil, no-op) when DATABASE_URL is unset so cmd/server keeps
// booting cleanly in health-only / smoke modes (same fail-soft pattern
// as buildIAMHandler / buildWebContactsHandler).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgfunnel "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnel"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/inbox"
	webfunnel "github.com/pericles-luz/crm/internal/web/funnel"
)

// buildWebFunnelHandler returns the HTMX funnel mux + a cleanup closure
// that releases the pgxpool. A nil handler signals "skip mounting" so
// callers can defer the cleanup unconditionally.
func buildWebFunnelHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/funnel handler disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/funnel handler disabled — pg connect: %v", err)
		return nil, noop
	}
	funnelStore, err := pgfunnel.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/funnel handler disabled — funnel store: %v", err)
		return nil, noop
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/funnel handler disabled — inbox store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebFunnelHandler(funnelStore, funnelStore, funnelStore, inboxStore, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/funnel handler disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/funnel HTMX routes mounted on public listener")
	return handler, func() { pool.Close() }
}

// assembleWebFunnelHandler builds the funnel.Service + web/funnel.Handler
// stack from already-built ports. Splitting the assembly out lets tests
// drive the wire with in-memory fakes without touching pgx.
//
// The three funnel-side ports (stages, transitions, board) are passed as
// separate parameters so a test can swap any individual port; in
// production all three are the same *pgfunnel.Store value.
func assembleWebFunnelHandler(
	stages funnel.StageRepository,
	transitions funnel.TransitionRepository,
	board funnel.BoardReader,
	assignments assignmentHistoryReader,
	logger *slog.Logger,
) (http.Handler, error) {
	if stages == nil {
		return nil, errors.New("funnel_wire: stages port is nil")
	}
	if transitions == nil {
		return nil, errors.New("funnel_wire: transitions port is nil")
	}
	if board == nil {
		return nil, errors.New("funnel_wire: board port is nil")
	}
	if assignments == nil {
		return nil, errors.New("funnel_wire: assignments port is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	svc, err := funnel.NewService(funnel.Config{
		Stages:      stages,
		Transitions: transitions,
		Publisher:   slogFunnelPublisher{logger: logger},
	})
	if err != nil {
		return nil, fmt.Errorf("funnel_wire: build service: %w", err)
	}
	h, err := webfunnel.New(webfunnel.Deps{
		Mover:             svc,
		Board:             board,
		StageResolver:     stages,
		FunnelHistory:     transitionsHistoryAdapter{port: transitions},
		AssignmentHistory: inboxAssignmentHistory{port: assignments},
		CSRFToken:         csrfTokenFromSessionContext,
		UserID:            userIDFromSessionContext,
		Logger:            logger,
	})
	if err != nil {
		return nil, fmt.Errorf("funnel_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// assignmentHistoryReader is the storage-port subset the funnel wire
// needs from inbox.Store: the assignment ledger projection keyed by
// (tenantID, conversationID). Declared here (composition root) so the
// adapter mapping into webfunnel.AssignmentEntry doesn't pull the inbox
// import into internal/web/funnel (forbidwebboundary lens, SIN-62735 /
// SIN-62862 AC).
type assignmentHistoryReader interface {
	ListHistory(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*inbox.Assignment, error)
}

// inboxAssignmentHistory adapts an inbox.AssignmentRepository ListHistory
// reader into webfunnel.AssignmentHistoryLister by remapping each row
// into the web-funnel-owned AssignmentEntry shape. The mapping is
// intentionally trivial — UserID + AssignedAt + Reason are all the
// history modal renders today.
type inboxAssignmentHistory struct {
	port assignmentHistoryReader
}

func (a inboxAssignmentHistory) ListHistory(ctx context.Context, tenantID, conversationID uuid.UUID) ([]webfunnel.AssignmentEntry, error) {
	rows, err := a.port.ListHistory(ctx, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	out := make([]webfunnel.AssignmentEntry, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, webfunnel.AssignmentEntry{
			AssignedAt: r.AssignedAt,
			UserID:     r.UserID,
			Reason:     string(r.Reason),
		})
	}
	return out, nil
}

// transitionsHistoryAdapter narrows funnel.TransitionRepository down to
// just ListForConversation — the only method webfunnel.FunnelHistoryLister
// needs. The concrete *pgfunnel.Store satisfies the wider port; we
// adapt instead of widening the web port so the read+write surfaces stay
// minimal and separately mockable.
type transitionsHistoryAdapter struct {
	port funnel.TransitionRepository
}

func (a transitionsHistoryAdapter) ListForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*funnel.Transition, error) {
	return a.port.ListForConversation(ctx, tenantID, conversationID)
}

// slogFunnelPublisher is the default funnel.EventPublisher used at boot.
// It writes one structured log line per published event so operators see
// transitions in the request log; downstream consumers (audit log,
// real-time UI refresh) hook in via a richer adapter when SIN-62194's
// message-bus wiring lands. The funnel_transition row in Postgres is the
// source of truth — Publish failures here are non-fatal by contract.
type slogFunnelPublisher struct {
	logger *slog.Logger
}

func (p slogFunnelPublisher) Publish(_ context.Context, eventName string, payload any) error {
	p.logger.Info("funnel: event published", "event", eventName, "payload", payload)
	return nil
}

// userIDFromSessionContext returns the session user id installed by
// middleware.Auth. uuid.Nil surfaces to the funnel handler as a 401
// (its MoveConversation contract requires a non-nil actor).
func userIDFromSessionContext(r *http.Request) uuid.UUID {
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		return uuid.Nil
	}
	return sess.UserID
}
