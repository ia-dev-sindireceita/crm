package main

// SIN-62765 — close F10 (MEDIUM) of SIN-62230 by wiring the
// SIN-62254 authz.AuditingAuthorizer into the live boot path. The
// decorator wraps the RBAC inner authorizer so every deny lands in
// audit_log_security at 100% and a sampled fraction of allows
// (default 1%) does too, per ADR 0004 §6.
//
// The sampling rate is read from AUTHZ_ALLOW_SAMPLE_RATE at boot so
// oncall can dial it up during incident response without redeploying.

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

const (
	// envAuthzAllowSampleRate names the env var that overrides the
	// allow-decision sampling rate. Parsed once at boot — runtime
	// mutation is intentionally out of scope (see SIN-62765 scope).
	envAuthzAllowSampleRate = "AUTHZ_ALLOW_SAMPLE_RATE"

	// defaultAuthzAllowSampleRate is the ADR 0004 §6 production
	// baseline (1%). Picked low enough that audit_log_security growth
	// is dominated by denies (which is the security signal we want)
	// rather than happy-path allows.
	defaultAuthzAllowSampleRate = 0.01
)

// parseAuthzAllowSampleRate returns the boot-time allow-sampling rate.
// An empty env var falls back to the production default; a malformed
// value logs a warning and also falls back, so a typo in deploy config
// never silently turns audit retention all the way up to 100%.
func parseAuthzAllowSampleRate(getenv func(string) string) float64 {
	raw := getenv(envAuthzAllowSampleRate)
	if raw == "" {
		return defaultAuthzAllowSampleRate
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("crm: invalid %s=%q, falling back to %v: %v",
			envAuthzAllowSampleRate, raw, defaultAuthzAllowSampleRate, err)
		return defaultAuthzAllowSampleRate
	}
	return v
}

// newAuditedAuthorizer assembles the production authz wrapper:
//
//   - RBAC inner (the verdict authority — ADR 0090 matrix).
//   - Recorder writing into audit_log_security via the split logger.
//   - Prometheus metrics on the shared registerer so the existing
//     /metrics endpoint exposes authz_decisions_total and
//     authz_user_deny_total alongside http_requests_total et al.
//   - Deterministic sampler keyed by request_id at the
//     AUTHZ_ALLOW_SAMPLE_RATE rate.
//
// pool is the postgres pool the recorder writes through. registerer
// is the seam tests use to avoid touching the global Prometheus
// registry. The decorator panics at construction on nil Inner /
// Recorder so a wireup mistake fails loudly at boot rather than
// silently degrading audit coverage.
func newAuditedAuthorizer(
	pool postgresadapter.AuditExecutor,
	registerer prometheus.Registerer,
	getenv func(string) string,
	logger *slog.Logger,
) (iam.Authorizer, error) {
	if pool == nil {
		return nil, errors.New("iam_authz_wire: pool is nil")
	}
	splitLogger, err := postgresadapter.NewSplitAuditLogger(pool)
	if err != nil {
		return nil, fmt.Errorf("iam_authz_wire: split audit logger: %w", err)
	}
	metrics := authz.NewMetrics(registerer)
	recorder := authz.NewAuditRecorder(splitLogger, metrics, logger)
	sampler := authz.NewDeterministicSampler(parseAuthzAllowSampleRate(getenv))
	inner := iam.NewRBACAuthorizer(iam.RBACConfig{})
	return authz.New(authz.Config{
		Inner:    inner,
		Recorder: recorder,
		Sampler:  sampler,
	}), nil
}

// Compile-time check: *pgxpool.Pool satisfies the AuditExecutor surface
// newAuditedAuthorizer accepts. Keeping the type-assertion here means a
// future change to either side breaks the build, not boot.
var _ postgresadapter.AuditExecutor = (*pgxpool.Pool)(nil)
