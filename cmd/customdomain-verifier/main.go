// customdomain-verifier is the standalone process described in
// SIN-63080 (Fase 5 / White-label avançado / DNS-poller): it polls
// tenant_custom_domains for rows in the implicit pending_dns state,
// invokes customdomain/management.UseCase.Verify on each one, and lets
// the use-case flip verified_at when the TXT record published by the
// tenant matches the stored verification token. The worker exists so
// users do not have to keep clicking "Verificar agora" in the UI after
// DNS propagates.
//
// The verifier package (internal/worker/customdomain_verifier) owns the
// orchestration: tick scheduling, per-domain exponential backoff,
// in-memory attempt cap, and the Prometheus metric surface. This
// entrypoint only translates the environment into ports and adapters
// and blocks on SIGINT / SIGTERM until shutdown.
//
// Configuration is read from the environment (12-factor):
//
//	DATABASE_URL                          mandatory — runtime pool DSN.
//	CUSTOMDOMAIN_VERIFIER_ENABLED         optional, default 1. Set to 0
//	                                      to leave the binary running but
//	                                      skip the polling loop (rollback
//	                                      switch — the binary still
//	                                      compiles + boots + exposes
//	                                      metrics, just no I/O).
//	CUSTOMDOMAIN_VERIFIER_INTERVAL        optional Go duration, default 60s.
//	CUSTOMDOMAIN_VERIFIER_MAX_ATTEMPTS    optional integer, default 720
//	                                      (12h at 60s).
//	CUSTOMDOMAIN_VERIFIER_INITIAL_BACKOFF optional Go duration, default 60s.
//	CUSTOMDOMAIN_VERIFIER_MAX_BACKOFF     optional Go duration, default 30m.
//	CUSTOMDOMAIN_VERIFIER_METRICS_ADDR    optional listen addr, default :9405.
//
// DNS resolver wiring mirrors cmd/server (SIN-62313 / ADR 0079 §2):
//
//	CUSTOMDOMAIN_DNS_SERVER  optional, points the miekg resolver at the
//	                         Unbound sidecar. Empty => stdlib resolver.
//	CUSTOMDOMAIN_DNSSEC      "1" (default) toggles the DNSSEC AD-bit
//	                         observability surface.
//
// Shutdown contract: SIGINT and SIGTERM cancel the root context;
// verifier.Run returns context.Canceled (mapped to a normal exit) and
// the metrics server's graceful shutdown deadline is 5s.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	miekgresolver "github.com/pericles-luz/crm/adapters/dnsresolver/miekg"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
	verifier "github.com/pericles-luz/crm/internal/worker/customdomain_verifier"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("customdomain-verifier exited", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgpool.NewFromEnv(ctx, os.Getenv)
	if err != nil {
		return fmt.Errorf("pg connect: %w", err)
	}
	defer pool.Close()

	store := pgstore.NewCustomDomainStore(pool)
	return runWith(ctx, logger, cfg, store, store, defaultResolverFactory)
}

// runWith is the testable boundary: production wires the real pgxpool-
// backed store, tests pass a fake. Splitting this out lets a unit test
// exercise the boot path (worker.New + metrics server + flag-off
// shortcut) without dialling Postgres.
func runWith(
	ctx context.Context,
	logger *slog.Logger,
	cfg config,
	managementStore management.Store,
	workerStore verifier.Store,
	resolverFactory func() dnsresolver.Resolver,
) error {
	reg := prometheus.NewRegistry()
	metrics := verifier.NewMetrics(reg)

	uc, err := assembleUseCase(managementStore, logger, resolverFactory)
	if err != nil {
		return fmt.Errorf("management: %w", err)
	}

	w, err := verifier.New(verifier.Config{
		Store:          workerStore,
		Verifier:       uc,
		Audit:          slogVerifierAudit{logger: logger},
		Logger:         logger,
		Metrics:        metrics,
		Interval:       cfg.interval,
		MaxAttempts:    cfg.maxAttempts,
		InitialBackoff: cfg.initialBackoff,
		MaxBackoff:     cfg.maxBackoff,
	})
	if err != nil {
		return fmt.Errorf("verifier.New: %w", err)
	}

	metricsSrv := &http.Server{
		Addr: cfg.metricsAddr,
		Handler: metricsMux(promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			Registry:          reg,
			EnableOpenMetrics: false,
		})),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("customdomain-verifier: metrics server crashed", "err", err.Error())
		}
	}()
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("customdomain-verifier starting",
		"enabled", cfg.enabled,
		"interval", cfg.interval.String(),
		"max_attempts", cfg.maxAttempts,
		"initial_backoff", cfg.initialBackoff.String(),
		"max_backoff", cfg.maxBackoff.String(),
		"metrics_addr", cfg.metricsAddr,
	)

	if !cfg.enabled {
		logger.Warn("customdomain-verifier: feature flag off; sleeping until ctx cancelled")
		<-ctx.Done()
		return ctx.Err()
	}

	return runner(ctx, w)
}

// runner is the test seam: production calls verifier.Worker.Run; tests
// substitute a fake that returns immediately so the wire-up path is
// exercised without spinning a goroutine on a real database.
var runner = func(ctx context.Context, w *verifier.Worker) error {
	return w.Run(ctx)
}

// assembleUseCase wires the management.UseCase the verifier needs. The
// worker only invokes Verify, so we only need a real DNS checker; the
// Validator (used by Enroll) is wired too for parity with cmd/server in
// case management.UseCase ever uses the validator inside Verify. The
// EnrollmentGate is a deny-by-default placeholder: Enroll is not
// invoked by this binary, so a refusing gate is the safest default.
//
// resolverFactory is a test seam: production passes
// defaultResolverFactory which binds the miekg adapter to
// CUSTOMDOMAIN_DNS_SERVER; tests inject a deterministic in-memory
// resolver so the wiring path can be exercised without DNS.
func assembleUseCase(store management.Store, logger *slog.Logger, resolverFactory func() dnsresolver.Resolver) (*management.UseCase, error) {
	validator := validation.New(
		resolverFactory(),
		validationAudit{logger: logger},
		validation.SystemClock{},
	)
	return management.New(management.Config{
		Store:     store,
		Gate:      denyEnrollmentGate{},
		Validator: hostValidatorAdapter{v: validator},
		DNS:       dnsCheckerAdapter{v: validator},
		Audit:     managementAudit{logger: logger},
		Now:       time.Now,
	})
}

// defaultResolverFactory returns the production miekg-backed resolver.
// CUSTOMDOMAIN_DNS_SERVER selects the recursive resolver (typically the
// Unbound sidecar in production per ADR 0079 §2); CUSTOMDOMAIN_DNSSEC
// toggles the AD-bit observability.
func defaultResolverFactory() dnsresolver.Resolver {
	server := os.Getenv("CUSTOMDOMAIN_DNS_SERVER")
	dnssec := true
	if v := os.Getenv("CUSTOMDOMAIN_DNSSEC"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			dnssec = parsed
		}
	}
	return miekgresolver.NewResolver(miekgresolver.Config{
		Server:       server,
		EnableDNSSEC: dnssec,
	})
}

// metricsMux exposes the registry on /metrics + /healthz so a probe can
// confirm the process is alive even when the polling loop is sleeping.
func metricsMux(metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// config bundles the parsed env. Required fields stay unexported so
// callers go through loadConfig (which validates).
type config struct {
	enabled        bool
	interval       time.Duration
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	metricsAddr    string
}

func loadConfig() (config, error) {
	c := config{
		enabled:        envBoolDefault("CUSTOMDOMAIN_VERIFIER_ENABLED", true),
		interval:       verifier.DefaultInterval,
		maxAttempts:    verifier.DefaultMaxAttempts,
		initialBackoff: verifier.DefaultInitialBackoff,
		maxBackoff:     verifier.DefaultMaxBackoff,
		metricsAddr:    envOr("CUSTOMDOMAIN_VERIFIER_METRICS_ADDR", ":9405"),
	}

	if os.Getenv(pgpool.EnvDSN) == "" {
		return c, fmt.Errorf("missing required env: %s", pgpool.EnvDSN)
	}

	if v := os.Getenv("CUSTOMDOMAIN_VERIFIER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("CUSTOMDOMAIN_VERIFIER_INTERVAL %q: must be positive Go duration", v)
		}
		c.interval = d
	}
	if v := os.Getenv("CUSTOMDOMAIN_VERIFIER_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("CUSTOMDOMAIN_VERIFIER_MAX_ATTEMPTS %q: must be positive integer", v)
		}
		c.maxAttempts = n
	}
	if v := os.Getenv("CUSTOMDOMAIN_VERIFIER_INITIAL_BACKOFF"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("CUSTOMDOMAIN_VERIFIER_INITIAL_BACKOFF %q: must be positive Go duration", v)
		}
		c.initialBackoff = d
	}
	if v := os.Getenv("CUSTOMDOMAIN_VERIFIER_MAX_BACKOFF"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("CUSTOMDOMAIN_VERIFIER_MAX_BACKOFF %q: must be positive Go duration", v)
		}
		c.maxBackoff = d
	}

	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBoolDefault(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// denyEnrollmentGate is the placeholder Gate the worker passes to
// management.New. The use-case requires a Gate but the worker only
// calls Verify, so the gate's Allow path is never reached in this
// binary. Returning Allowed=false makes a hypothetical accidental call
// fail closed.
type denyEnrollmentGate struct{}

func (denyEnrollmentGate) Allow(context.Context, uuid.UUID) management.EnrollmentDecision {
	return management.EnrollmentDecision{
		Allowed: false,
		Err:     errors.New("customdomain-verifier: enrollment gate disabled in this binary"),
	}
}

// slogVerifierAudit forwards GiveUpEvent to slog so the failed-cap
// transition lands in the same log pipeline as the management events.
type slogVerifierAudit struct{ logger *slog.Logger }

func (a slogVerifierAudit) LogVerifierGiveUp(ctx context.Context, ev verifier.GiveUpEvent) {
	a.logger.LogAttrs(ctx, slog.LevelWarn, "customdomain.verifier.giveup",
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("domain_id", ev.DomainID.String()),
		slog.String("host", ev.Host),
		slog.String("reason", ev.Reason),
		slog.Int("attempts", ev.Attempts),
		slog.Time("at", ev.At),
	)
}

// managementAudit + validationAudit + hostValidatorAdapter +
// dnsCheckerAdapter mirror cmd/server/customdomain_wire.go. Re-declared
// here to keep the worker binary free of any cmd/server import — they
// are small enough that duplication beats a shared package.
type managementAudit struct{ logger *slog.Logger }

func (a managementAudit) LogManagement(ctx context.Context, ev management.AuditEvent) {
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.management",
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("domain_id", ev.DomainID.String()),
		slog.String("host", ev.Host),
		slog.String("action", ev.Action),
		slog.String("outcome", ev.Outcome),
		slog.String("reason", ev.Reason.String()),
	)
}

type validationAudit struct{ logger *slog.Logger }

func (a validationAudit) Record(ctx context.Context, ev validation.AuditEvent) {
	attrs := []slog.Attr{
		slog.String("event", ev.Event),
		slog.String("host", ev.Host),
		slog.Time("at", ev.At),
	}
	for k, v := range ev.Detail {
		attrs = append(attrs, slog.String("detail."+k, v))
	}
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.validation", attrs...)
}

type hostValidatorAdapter struct{ v *validation.Validator }

func (h hostValidatorAdapter) Validate(ctx context.Context, host string) error {
	if err := h.v.ValidateHostOnly(ctx, host); err != nil {
		switch {
		case errors.Is(err, validation.ErrPrivateIP):
			return fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		case errors.Is(err, validation.ErrEmptyHost):
			return fmt.Errorf("%w: %w", management.ErrInvalidHost, err)
		default:
			return err
		}
	}
	return nil
}

type dnsCheckerAdapter struct{ v *validation.Validator }

func (d dnsCheckerAdapter) Check(ctx context.Context, host, expectedToken string) (management.DNSCheckResult, error) {
	res, err := d.v.Validate(ctx, host, expectedToken)
	if err != nil {
		switch {
		case errors.Is(err, validation.ErrTokenMismatch):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrTokenMismatch, err)
		case errors.Is(err, validation.ErrPrivateIP):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		default:
			return management.DNSCheckResult{}, err
		}
	}
	return management.DNSCheckResult{WithDNSSEC: res.VerifiedWithDNSSEC}, nil
}
