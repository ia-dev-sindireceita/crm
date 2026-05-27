package main

// SIN-63605 — wire constructor for the 4-eyes approval surface
// (over-cap grants). The constructor builds the postgres adapter
// (over the master_ops pool) and the five httpapi handlers from
// internal/web/master. POST verbs are wrapped with
// mastermfa.RequireRecentMFA(15m) so a recent MFA assertion gates
// every create/approve/reject call.
//
// The slots are inserted into httpapi.MasterTenantsRoutes when
// cmd/server's primary wire (iam_wire.go) supplies the master_ops
// pool, MasterSessionRecentMFA adapter, and CSRF token resolver.
// The constructor is exported (BuildMasterGrantRequestsRoutes) but
// the cmd/server NewRouter call only populates the slots when both
// the C9/C10 master surface AND the SIN-63605 dependencies are
// available — without the master surface wired up, the GET / POST
// routes for the request flow have no peer to live alongside.
// Until that lands, the slots stay nil and the router skips them
// (zero-value MasterTenantsRoutes is the existing pre-PR behaviour).
//
// New env knobs intentionally absent: the 15m freshness window is
// the ADR-0074 §D3 constant and matches the C10 grant POSTs; making
// it tunable per-deploy would defeat the policy. cmd/server tests
// override the constant by injecting a configured constructor.

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// recentMFAWindow is the ADR-0074 §D3 freshness window applied to the
// create/approve/reject POSTs. Matches the per-grant freshness gate on
// the C10 routes (see SIN-62884 / ADR-0098 §D3).
const recentMFAWindow = 15 * time.Minute

// MasterGrantRequestsDeps bundles every collaborator the constructor
// needs. cmd/server (iam_wire.go) supplies these once the master
// surface adapters are available; today the constructor returns a
// zero-value MasterTenantsRoutes slot set if any required dep is nil,
// which means the router safely skips the new routes.
type MasterGrantRequestsDeps struct {
	MasterOpsPool     *pgxpool.Pool
	ActorID           uuid.UUID
	RecentMFASessions mastermfa.MasterSessionRecentMFA
	CSRFToken         masterweb.CSRFTokenFn
	Logger            *slog.Logger
	WebMasterDeps     masterweb.Deps
}

// MasterGrantRequestsRoutes is the slot-aware projection the wire
// caller plugs into httpapi.MasterTenantsRoutes. Each handler is
// already pre-wrapped with mastermfa.RequireRecentMFA where the ADR
// requires it; the router still adds RequireAuth + RequireAction on
// top.
type MasterGrantRequestsRoutes struct {
	Create  http.Handler
	List    http.Handler
	Show    http.Handler
	Approve http.Handler
	Reject  http.Handler
}

// ErrMasterGrantRequestsDepsMissing is returned by
// BuildMasterGrantRequestsRoutes when a required dep is nil/zero so
// cmd/server can fail boot rather than mount a half-wired surface.
var ErrMasterGrantRequestsDepsMissing = errors.New("cmd/server: master grant requests dependencies missing")

// BuildMasterGrantRequestsRoutes constructs the adapter, the master
// web handler bundle, and wraps the POST verbs with
// mastermfa.RequireRecentMFA(15m). The returned Routes are
// designed to drop straight into httpapi.MasterTenantsRoutes.
//
// The function does NOT mutate the input Deps and never returns
// partially-populated Routes — every field is either all-set or
// the function returns ErrMasterGrantRequestsDepsMissing.
func BuildMasterGrantRequestsRoutes(deps MasterGrantRequestsDeps) (MasterGrantRequestsRoutes, error) {
	if deps.MasterOpsPool == nil || deps.ActorID == uuid.Nil ||
		deps.RecentMFASessions == nil || deps.CSRFToken == nil {
		return MasterGrantRequestsRoutes{}, ErrMasterGrantRequestsDepsMissing
	}
	store, err := masterpg.NewGrantRequestStore(deps.MasterOpsPool, deps.ActorID)
	if err != nil {
		return MasterGrantRequestsRoutes{}, err
	}
	webDeps := deps.WebMasterDeps
	webDeps.GrantRequests = store
	if webDeps.CSRFToken == nil {
		webDeps.CSRFToken = deps.CSRFToken
	}
	if webDeps.Logger == nil {
		webDeps.Logger = deps.Logger
	}
	h, err := masterweb.New(webDeps)
	if err != nil {
		return MasterGrantRequestsRoutes{}, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	recentMW := mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{
		Sessions: deps.RecentMFASessions,
		MaxAge:   recentMFAWindow,
		Logger:   logger,
	})
	return MasterGrantRequestsRoutes{
		Create:  recentMW(http.HandlerFunc(h.CreateGrantRequest)),
		List:    http.HandlerFunc(h.ListGrantRequests),
		Show:    http.HandlerFunc(h.ShowGrantRequest),
		Approve: recentMW(http.HandlerFunc(h.ApproveGrantRequest)),
		Reject:  recentMW(http.HandlerFunc(h.RejectGrantRequest)),
	}, nil
}

// applyToMasterTenantsRoutes copies the populated slots from r into
// dst. Safe to call with the zero Routes (no-op) so cmd/server can
// always invoke it after BuildMasterGrantRequestsRoutes regardless of
// whether the deps were available.
func (r MasterGrantRequestsRoutes) applyToMasterTenantsRoutes(dst *httpapi.MasterTenantsRoutes) {
	if r.Create != nil {
		dst.GrantRequestsCreate = r.Create
	}
	if r.List != nil {
		dst.GrantRequestsList = r.List
	}
	if r.Show != nil {
		dst.GrantRequestsShow = r.Show
	}
	if r.Approve != nil {
		dst.GrantRequestsApprove = r.Approve
	}
	if r.Reject != nil {
		dst.GrantRequestsReject = r.Reject
	}
}
