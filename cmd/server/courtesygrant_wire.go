package main

// SIN-62730 — constructor + env parsing for the on-tenant-creation
// courtesy-grant flow (Fase 1 PR11). The pieces here are wired but
// NOT yet mounted, because the master tenant-create endpoint
// (action `master.tenant.create`) is itself a future ticket. Once
// that flow lands, its use-case takes a tenancy.CourtesyGrantIssuer
// dependency and the wiring at runWith hands it the service this
// file constructs.
//
// Splitting the wire-up from the consumer keeps PR11 reviewable in
// isolation: the four-statement onboarding transaction, its
// adapter, the env knobs (COURTESY_GRANT_TOKENS,
// COURTESY_GRANT_DISABLED, COURTESY_GRANT_ACTOR_ID), and the
// idempotency tests all ship together, with no half-wired path
// reaching production traffic.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/tenancy"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
)

const (
	envCourtesyAmount   = "COURTESY_GRANT_TOKENS"
	envCourtesyDisabled = "COURTESY_GRANT_DISABLED"
	envCourtesyActor    = "COURTESY_GRANT_ACTOR_ID"

	// defaultCourtesyAmount is the value used when the env var is
	// unset or unparseable. 10_000 tokens covers a few hundred
	// outbound WhatsApp messages at MVP prices — enough for a tenant
	// to validate the product before paying.
	defaultCourtesyAmount int64 = 10_000
)

// ErrCourtesyActorRequired flags a misconfigured deploy: when the
// courtesy flow is enabled, a master_ops actor uuid MUST be
// supplied via COURTESY_GRANT_ACTOR_ID so the audit chain stays
// non-null. Returned from buildCourtesyGrantConfig so cmd/server
// can fail boot rather than reach a runtime null actor.
var ErrCourtesyActorRequired = errors.New("courtesygrant: COURTESY_GRANT_ACTOR_ID required when flow is enabled")

// buildCourtesyGrantConfig parses the three env knobs into a
// wallet/usecase config. Disabled overrides every other field: when
// the flow is off, the actor id is allowed to be uuid.Nil so dev/CI
// images can boot without provisioning a master onboarding user.
func buildCourtesyGrantConfig(getenv func(string) string) (walletusecase.IssueCourtesyGrantConfig, error) {
	disabled := false
	if v := getenv(envCourtesyDisabled); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return walletusecase.IssueCourtesyGrantConfig{}, fmt.Errorf("courtesygrant: parse %s=%q: %w", envCourtesyDisabled, v, err)
		}
		disabled = parsed
	}

	amount := defaultCourtesyAmount
	if v := getenv(envCourtesyAmount); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return walletusecase.IssueCourtesyGrantConfig{}, fmt.Errorf("courtesygrant: parse %s=%q: %w", envCourtesyAmount, v, err)
		}
		if parsed <= 0 {
			return walletusecase.IssueCourtesyGrantConfig{}, fmt.Errorf("courtesygrant: %s must be positive (got %d)", envCourtesyAmount, parsed)
		}
		amount = parsed
	}

	var actor uuid.UUID
	if v := getenv(envCourtesyActor); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			return walletusecase.IssueCourtesyGrantConfig{}, fmt.Errorf("courtesygrant: parse %s=%q: %w", envCourtesyActor, v, err)
		}
		actor = parsed
	}
	if !disabled && actor == uuid.Nil {
		return walletusecase.IssueCourtesyGrantConfig{}, ErrCourtesyActorRequired
	}
	return walletusecase.IssueCourtesyGrantConfig{
		Amount:   amount,
		Disabled: disabled,
		ActorID:  actor,
	}, nil
}

// buildCourtesyGrantService wires the master_ops pool into the
// adapter and constructs the use-case service. Returns nil with a
// nil error when the flow is disabled — callers then mount a no-op
// or skip the bootstrap call entirely.
func buildCourtesyGrantService(masterOpsPool *pgxpool.Pool, cfg walletusecase.IssueCourtesyGrantConfig) (*walletusecase.IssueCourtesyGrantService, error) {
	if cfg.Disabled {
		return nil, nil
	}
	store, err := walletadapter.NewCourtesyStore(masterOpsPool)
	if err != nil {
		return nil, fmt.Errorf("courtesygrant: build store: %w", err)
	}
	svc, err := walletusecase.NewIssueCourtesyGrantService(store, cfg)
	if err != nil {
		return nil, fmt.Errorf("courtesygrant: build service: %w", err)
	}
	return svc, nil
}

// courtesyIssuerAdapter projects *IssueCourtesyGrantService onto the
// consumer-side tenancy port. The adapter swallows
// wallet.ErrCourtesyGrantDisabled so a disabled flow is invisible to
// the tenant-create flow (it just sees "no error, no grant").
type courtesyIssuerAdapter struct {
	svc *walletusecase.IssueCourtesyGrantService
}

// IssueCourtesyGrant satisfies tenancy.CourtesyGrantIssuer.
func (a courtesyIssuerAdapter) IssueCourtesyGrant(ctx context.Context, tenantID uuid.UUID) error {
	if a.svc == nil {
		return nil
	}
	_, err := a.svc.Issue(ctx, tenantID)
	return err
}

// Compile-time guard.
var _ tenancy.CourtesyGrantIssuer = courtesyIssuerAdapter{}
