package main

// SIN-63958 — wire constructor for the session-bound impersonation envelope
// (master-impersonation-spec §1). Assembles the ImpersonationStore, the three
// handlers (Start / End / Feed), and the ImpersonationFromSession middleware
// into an httpapi.ImpersonationRoutes bundle ready to drop into httpapi.Deps.
//
// Pattern mirrors lgpd_wire.go: opens its own pgxpool against
// MASTER_OPS_DATABASE_URL, returns a noopImpersonationStack() on any missing
// or invalid input, and exposes a Cleanup func so cmd/server can always defer
// it without a nil-check.

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// impersonationStack bundles the routes and a cleanup hook.
type impersonationStack struct {
	Routes  httpapi.ImpersonationRoutes
	Cleanup func()
}

func noopImpersonationStack() impersonationStack {
	return impersonationStack{Cleanup: func() {}}
}

// buildImpersonationStack assembles the SIN-63958 impersonation surface.
// pool is the IAM runtime pool (for SplitAuditLogger). resolver is the
// tenancy.ByIDResolver already wired by buildIAMHandler.
//
// Returns noopImpersonationStack() when MASTER_OPS_DATABASE_URL is unset or
// any constructor fails.
func buildImpersonationStack(
	ctx context.Context,
	iamPool *pgxpool.Pool,
	resolver tenancy.ByIDResolver,
	getenv func(string) string,
) impersonationStack {
	if iamPool == nil || resolver == nil {
		return noopImpersonationStack()
	}
	masterDSN := getenv(envMasterOpsDSN)
	if masterDSN == "" {
		log.Printf("crm: impersonation envelope disabled (%s unset)", envMasterOpsDSN)
		return noopImpersonationStack()
	}

	masterPool, err := pgxpool.New(ctx, masterDSN)
	if err != nil {
		log.Printf("crm: impersonation envelope disabled — master pg connect: %v", err)
		return noopImpersonationStack()
	}

	impStore, err := masterpg.NewImpersonationStore(masterPool)
	if err != nil {
		masterPool.Close()
		log.Printf("crm: impersonation envelope disabled — store: %v", err)
		return noopImpersonationStack()
	}

	auditLogger, err := postgresadapter.NewSplitAuditLogger(iamPool)
	if err != nil {
		masterPool.Close()
		log.Printf("crm: impersonation envelope disabled — audit logger: %v", err)
		return noopImpersonationStack()
	}

	checker := &pgMasterChecker{pool: masterPool}

	mw := middleware.ImpersonationFromSession(
		checker,
		resolver,
		impStore,
		auditLogger,
		func() time.Time { return time.Now().UTC() },
		slog.Default(),
	)

	impHandler, err := masterweb.NewImpersonationHandler(masterweb.ImpersonationDeps{
		Sessions: impStore,
		Auditor:  auditLogger,
		Tenants:  resolver,
		Logger:   slog.Default(),
		Clock:    func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		masterPool.Close()
		log.Printf("crm: impersonation envelope disabled — handler: %v", err)
		return noopImpersonationStack()
	}

	return impersonationStack{
		Routes: httpapi.ImpersonationRoutes{
			Start:       impHandler.StartHandler(),
			End:         impHandler.EndHandler(),
			Feed:        impHandler.FeedHandler(),
			FromSession: mw,
		},
		Cleanup: masterPool.Close,
	}
}

// pgMasterChecker implements middleware.MasterChecker using the master_ops
// pool. It queries the users table (bypassing tenant RLS via WithMasterOps)
// to check whether the given user ID carries role='master'.
//
// The actorID passed to WithMasterOps is the userID being checked — this is a
// read-only verification so the user themselves is the appropriate audit actor.
type pgMasterChecker struct {
	pool postgresadapter.TxBeginner
}

func (c *pgMasterChecker) IsMaster(ctx context.Context, userID uuid.UUID) (bool, error) {
	if userID == uuid.Nil {
		return false, nil
	}
	// SIN-66305 gate 2 (defense in depth): the reserved system principal is
	// seeded role='master' (it is a master-context row), so refuse it here
	// before the role read — it must never be honoured as an impersonation
	// master, closing the is_master amplification vector even though it can
	// never authenticate (gate 1) to reach this check.
	if iam.IsSystemPrincipal(userID) {
		return false, nil
	}
	var roleStr string
	err := postgresadapter.WithMasterOps(ctx, c.pool, userID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT role FROM users WHERE id = $1`, userID,
		).Scan(&roleStr)
	})
	if err != nil {
		return false, fmt.Errorf("impersonation: IsMaster check: %w", err)
	}
	return iam.Role(roleStr).IsMaster(), nil
}
