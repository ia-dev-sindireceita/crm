package main

// SIN-63957 / SIN-63955 Scope 1+2 — wire constructor for the master
// /master/tenants + /master/grants + /master/grants/requests surface
// (Fase 2.5 C9/C10 + SIN-63605 4-eyes approval).
//
// Mirrors the impersonation_wire.go shape: opens its own pgxpool against
// MASTER_OPS_DATABASE_URL, returns a noopMasterTenantsStack() on any
// missing or invalid input, and exposes a Cleanup func so cmd/server can
// always defer it without a nil-check. The router skips routes with nil
// slots so a deploy that hasn't provisioned the master_ops DSN or the
// master service-account actor keeps the rest of the surface working
// (reference_crm_router_nil_dep_silent_skip).
//
// All 11 mounted routes land here:
//
//   GET    /master/tenants                          - List
//   POST   /master/tenants                          - Create
//   PATCH  /master/tenants/{id}/plan                - AssignPlan
//   GET    /master/tenants/{id}/grants/new          - GrantsNew (ShowGrantsForm)
//   POST   /master/tenants/{id}/grants              - GrantsCreate (IssueGrant)   [recent-MFA]
//   POST   /master/grants/{id}/revoke               - GrantsRevoke (RevokeGrant)  [recent-MFA]
//   POST   /master/tenants/{id}/grants/requests     - GrantRequestsCreate          [recent-MFA via helper]
//   GET    /master/grants/requests                  - GrantRequestsList
//   GET    /master/grants/requests/{id}             - GrantRequestsShow
//   POST   /master/grants/requests/{id}/approve     - GrantRequestsApprove         [recent-MFA via helper]
//   POST   /master/grants/requests/{id}/reject      - GrantRequestsReject          [recent-MFA via helper]
//
// The GrantsCreate / GrantsRevoke wrapping happens here; the five
// grant-request slots are wrapped inside BuildMasterGrantRequestsRoutes
// per master_grant_requests_wire.go:111.

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	billingadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/wallet"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// envMasterOpsActorID names the master-side service-account user UUID
// stamped on the master_ops_audit trigger via WithMasterOps. The same
// value is consumed by every adapter built in this wire (tenant store,
// wallet grant store, mastersession store). Unset → the entire master
// surface stays nil and the router emits clean 404s on each path.
const envMasterOpsActorID = "MASTER_OPS_ACTOR_ID"

// masterTenantsStack carries the populated MasterTenantsRoutes and the
// pool cleanup hook so cmd/server can always defer Close without a
// nil-check.
type masterTenantsStack struct {
	Routes  httpapi.MasterTenantsRoutes
	Cleanup func()
}

func noopMasterTenantsStack() masterTenantsStack {
	return masterTenantsStack{Cleanup: func() {}}
}

// buildMasterTenantsStack assembles the SIN-63957 master surface.
// runtimePool is the IAM runtime pool (used for plan reads + audit
// writer); auditWriter is the SplitAuditLogger already wired by the
// caller so master.grant.issued events join the same audit chain as the
// rest of the security ledger.
//
// Returns noopMasterTenantsStack() on any missing input or constructor
// failure; the failure mode is "this surface stays unmounted" which
// matches the existing pre-PR behaviour (every slot nil → silent 404).
func buildMasterTenantsStack(
	ctx context.Context,
	runtimePool *pgxpool.Pool,
	auditWriter audit.SplitLogger,
	getenv func(string) string,
	logger *slog.Logger,
	tenantsResolver tenancy.ByIDResolver,
) masterTenantsStack {
	if runtimePool == nil || auditWriter == nil || logger == nil {
		return noopMasterTenantsStack()
	}
	masterDSN := getenv(envMasterOpsDSN)
	if masterDSN == "" {
		log.Printf("crm: master tenants surface disabled (%s unset)", envMasterOpsDSN)
		return noopMasterTenantsStack()
	}
	actorRaw := getenv(envMasterOpsActorID)
	if actorRaw == "" {
		log.Printf("crm: master tenants surface disabled (%s unset)", envMasterOpsActorID)
		return noopMasterTenantsStack()
	}
	actorID, err := uuid.Parse(actorRaw)
	if err != nil || actorID == uuid.Nil {
		log.Printf("crm: master tenants surface disabled — invalid %s: %v", envMasterOpsActorID, err)
		return noopMasterTenantsStack()
	}

	masterOpsPool, err := pgxpool.New(ctx, masterDSN)
	if err != nil {
		log.Printf("crm: master tenants surface disabled — master pg connect: %v", err)
		return noopMasterTenantsStack()
	}
	cleanup := masterOpsPool.Close

	// Base tenant ports (List + Create + AssignPlan).
	tenantStore, err := masterpg.NewMasterTenantStore(masterOpsPool, runtimePool, actorID)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — tenant store: %v", err)
		return noopMasterTenantsStack()
	}

	// Plan catalogue (read-only) — shim over billing.Store.
	billingStore, err := billingadapter.New(runtimePool, masterOpsPool)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — billing store: %v", err)
		return noopMasterTenantsStack()
	}
	planLister, err := masterpg.NewPlanListerShim(billingStore)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — plan lister: %v", err)
		return noopMasterTenantsStack()
	}

	// Wallet grants surface (C10). The audited wrapper writes
	// master.grant.issued audit rows on Create (ADR-0098 §D3).
	walletGrantStore, err := walletadapter.NewMasterGrantStore(masterOpsPool, actorID)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — wallet grant store: %v", err)
		return noopMasterTenantsStack()
	}
	auditedGrantRepo, err := wallet.NewAuditedMasterGrantRepository(walletGrantStore, auditWriter, time.Now, logger)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — audited grant repo: %v", err)
		return noopMasterTenantsStack()
	}
	grantPort, err := masterweb.NewWalletGrantPort(auditedGrantRepo, time.Now)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — wallet grant port: %v", err)
		return noopMasterTenantsStack()
	}

	// Recent-MFA reader over the master session store. The shared
	// store is reused by RequireRecentMFA below and by the SIN-63605
	// helper (master_grant_requests_wire.go) so a single envelope
	// drives every gated POST verb on the surface.
	sessionStore, err := mastersession.New(masterOpsPool, actorID)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — master session store: %v", err)
		return noopMasterTenantsStack()
	}
	recentReader := mastermfa.NewRecentReader(sessionStore)

	// Build the masterweb.Handler. NB: BuildMasterGrantRequestsRoutes
	// constructs its own internal handler from the same webDeps + the
	// GrantRequests store it builds; the two handler instances share
	// the deps but own distinct method-set bindings, so the base 6
	// slots come from `h` and the 5 grant-request slots come from the
	// helper. See master_grant_requests_wire.go:95.
	webDeps := masterweb.Deps{
		Tenants:  tenantStore,
		Creator:  tenantStore,
		Plans:    planLister,
		Assigner: tenantStore,
		// SIN-65289: the relocated /master/* surface runs on the master-
		// host chain, which installs no tenant session, so the tenant
		// provider (csrfTokenFromSessionContext) returned "" → every GET
		// 500'd ("csrf token missing"). Read the master operator from the
		// master-session context instead. Origin CSRF (SIN-65269) remains
		// THE control for the POSTs; this token only renders the form.
		CSRFToken:       mastermfa.CSRFTokenFromContext,
		Logger:          logger,
		Grants:          grantPort,
		TenantsResolver: tenantsResolver,
	}
	h, err := masterweb.New(webDeps)
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — masterweb.New: %v", err)
		return noopMasterTenantsStack()
	}

	// SIN-62884 + ADR-0098 §D3: GrantsCreate / GrantsRevoke require a
	// recent MFA assertion. Window matches recentMFAWindow from the
	// grant-requests helper (master_grant_requests_wire.go:44) so a
	// single 15-minute envelope drives every gated POST on the
	// surface.
	recentMW := mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{
		Sessions: recentReader,
		MaxAge:   recentMFAWindow,
		Logger:   logger,
	})

	routes := httpapi.MasterTenantsRoutes{
		List:         http.HandlerFunc(h.ListTenants),
		Create:       http.HandlerFunc(h.CreateTenant),
		AssignPlan:   http.HandlerFunc(h.AssignPlan),
		Detail:       http.HandlerFunc(h.ShowTenantDetail),
		GrantsNew:    http.HandlerFunc(h.ShowGrantsForm),
		GrantsCreate: recentMW(http.HandlerFunc(h.IssueGrant)),
		GrantsRevoke: recentMW(http.HandlerFunc(h.RevokeGrant)),
	}

	grantReqRoutes, err := BuildMasterGrantRequestsRoutes(MasterGrantRequestsDeps{
		MasterOpsPool:     masterOpsPool,
		ActorID:           actorID,
		RecentMFASessions: recentReader,
		// SIN-65289: master-host chain has no tenant session — read the
		// master operator from the master-session context (see above).
		CSRFToken:     mastermfa.CSRFTokenFromContext,
		Logger:        logger,
		WebMasterDeps: webDeps,
	})
	if err != nil {
		cleanup()
		log.Printf("crm: master tenants surface disabled — grant requests: %v", err)
		return noopMasterTenantsStack()
	}
	grantReqRoutes.applyToMasterTenantsRoutes(&routes)

	return masterTenantsStack{
		Routes:  routes,
		Cleanup: cleanup,
	}
}
