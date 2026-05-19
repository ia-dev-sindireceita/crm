package customdomain_verifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// Defaults the worker falls back to when Config leaves a field zero.
const (
	// DefaultInterval is the inter-tick delay. 60s matches the SIN-63080
	// spec: short enough that a user who just clicked "Adicionar
	// domínio" sees the badge flip within a minute of TXT propagation,
	// long enough that a small backlog cannot starve the DB.
	DefaultInterval = 60 * time.Second

	// DefaultMaxAttempts is the in-memory cap before the worker marks a
	// row failed. 720 × 60s = 12h — the SIN-63080 spec value. The cap
	// resets on worker restart, which is an acceptable trade-off (a
	// freshly-restarted worker may retry a stale row up to N more times
	// before persisting the failure).
	DefaultMaxAttempts = 720

	// DefaultInitialBackoff is the wait after the first transient
	// failure before the worker retries the same domain.
	DefaultInitialBackoff = 60 * time.Second

	// DefaultMaxBackoff caps the exponential backoff so a row stuck on
	// mismatch never waits longer than this between attempts. Bounded
	// to keep the cap-counter (a per-attempt counter, not a per-second
	// counter) bound the worst-case time-to-failure.
	DefaultMaxBackoff = 30 * time.Minute

	// DefaultBackoffFactor is the multiplier applied between attempts.
	DefaultBackoffFactor = 2.0
)

// Config groups Worker dependencies. New returns an error if a
// non-optional field is missing and fills defaults for the rest.
type Config struct {
	// Store is the persistence port. Required.
	Store Store
	// Verifier is the management Verify port. Required.
	Verifier Verifier
	// Audit is the structured-event sink for give-up events. Optional.
	Audit AuditLogger
	// Logger is the structured logger. Defaults to slog.Default.
	Logger *slog.Logger
	// Metrics is the Prometheus surface. nil disables metrics.
	Metrics *Metrics

	// Interval is the inter-tick delay. Defaults to DefaultInterval.
	Interval time.Duration
	// MaxAttempts is the in-memory cap. Defaults to DefaultMaxAttempts.
	MaxAttempts int
	// InitialBackoff is the first transient-failure wait.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential backoff.
	MaxBackoff time.Duration
	// BackoffFactor is the exponential multiplier.
	BackoffFactor float64

	// Clock returns "now". Defaults to time.Now().UTC().
	Clock func() time.Time
}

// Worker is the DNS-poller. Construct once with New and call Run from
// the main goroutine. Run blocks until ctx is cancelled.
type Worker struct {
	store    Store
	verifier Verifier
	audit    AuditLogger
	logger   *slog.Logger
	metrics  *Metrics

	interval       time.Duration
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	backoffFactor  float64

	now func() time.Time

	mu       sync.Mutex
	progress map[uuid.UUID]*progressEntry
}

// progressEntry tracks the in-memory state for one pending domain
// across ticks: attempt count and the time it is eligible to be tried
// again. Both are reset implicitly when the entry is pruned (either
// after a successful Verify or once the row leaves the pending state).
type progressEntry struct {
	attempts  int
	nextTryAt time.Time
}

// New constructs a Worker from cfg. Required: Store, Verifier. Returns
// a descriptive error for missing pieces so a misconfigured wireup
// fails fast at boot.
func New(cfg Config) (*Worker, error) {
	if cfg.Store == nil {
		return nil, errors.New("customdomain_verifier: Store is required")
	}
	if cfg.Verifier == nil {
		return nil, errors.New("customdomain_verifier: Verifier is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = DefaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.BackoffFactor < 1 {
		cfg.BackoffFactor = DefaultBackoffFactor
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		store:          cfg.Store,
		verifier:       cfg.Verifier,
		audit:          cfg.Audit,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		interval:       cfg.Interval,
		maxAttempts:    cfg.MaxAttempts,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		backoffFactor:  cfg.BackoffFactor,
		now:            cfg.Clock,
		progress:       map[uuid.UUID]*progressEntry{},
	}, nil
}

// Run blocks until ctx is cancelled, ticking once per Interval. The
// first tick fires immediately so deployments do not wait Interval
// seconds before the first sweep.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	_ = w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = w.Tick(ctx)
		}
	}
}

// Tick performs one sweep. Exported so tests, integration harnesses,
// and CLI tools can drive the worker deterministically. The method
// records customdomain_verifier_cycle_duration_seconds for every call,
// including error paths.
func (w *Worker) Tick(ctx context.Context) error {
	started := w.now()
	w.metrics.incCycles()
	defer func() {
		w.metrics.observeCycle(w.now().Sub(started).Seconds())
	}()

	domains, err := w.store.ListPendingVerification(ctx)
	if err != nil {
		w.logger.Error("customdomain_verifier: list pending failed",
			slog.String("err", err.Error()),
		)
		return fmt.Errorf("list pending: %w", err)
	}
	w.metrics.setPending(len(domains))

	now := started
	w.pruneInactive(domains)
	for _, d := range domains {
		w.evaluate(ctx, d, now)
	}
	return nil
}

// evaluate processes one pending domain end-to-end: backoff gate →
// Verify → outcome classification → bookkeeping (backoff bump or
// success drop) → cap check.
func (w *Worker) evaluate(ctx context.Context, d management.Domain, now time.Time) {
	w.mu.Lock()
	entry, ok := w.progress[d.ID]
	if !ok {
		entry = &progressEntry{}
		w.progress[d.ID] = entry
	}
	if !entry.nextTryAt.IsZero() && now.Before(entry.nextTryAt) {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	logger := w.logger.With(
		slog.String("tenant_id", d.TenantID.String()),
		slog.String("domain_id", d.ID.String()),
		slog.String("host", d.Host),
	)

	outcome := w.runVerify(ctx, d, logger)
	w.metrics.incOutcome(outcome)

	switch outcome {
	case OutcomeVerified, OutcomeAlreadyVerified:
		w.dropProgress(d.ID)
		logger.Info("customdomain_verifier: verified",
			slog.String("outcome", outcome.String()),
		)
		return
	case OutcomeBlockedSSRF:
		// SSRF block does NOT count toward the cap — the user can fix
		// their DNS and the worker should keep polling. Schedule a
		// longer backoff so we do not log-spam the audit on every tick.
		w.scheduleBackoff(d.ID, now, true /*ssrfHold*/)
		logger.Warn("customdomain_verifier: blocked_ssrf",
			slog.String("outcome", outcome.String()),
		)
		return
	case OutcomeMismatch, OutcomeResolverError, OutcomeInternal:
		// Count this attempt and either schedule a retry or mark failed.
		w.bumpAttempt(d.ID)
		attempts := w.attemptsFor(d.ID)
		logger.Info("customdomain_verifier: transient failure",
			slog.String("outcome", outcome.String()),
			slog.Int("attempts", attempts),
		)
		if attempts >= w.maxAttempts {
			w.giveUp(ctx, d, attempts, now, logger)
			return
		}
		w.scheduleBackoff(d.ID, now, false /*ssrfHold*/)
		return
	}
}

// runVerify executes one Verify call and classifies its outcome. Errors
// returned by the use-case never bubble out of the worker — they are
// classified and counted via the Outcome.
func (w *Worker) runVerify(ctx context.Context, d management.Domain, logger *slog.Logger) Outcome {
	out, err := w.verifier.Verify(ctx, d.TenantID, d.ID)
	if err != nil {
		// Map the management.Reason already populated by the use-case
		// to an Outcome so the metric labels stay stable.
		switch out.Reason {
		case management.ReasonTokenMismatch:
			return OutcomeMismatch
		case management.ReasonPrivateIP:
			return OutcomeBlockedSSRF
		case management.ReasonDNSResolutionFailed:
			return OutcomeResolverError
		default:
			logger.Error("customdomain_verifier: verify error",
				slog.String("err", err.Error()),
				slog.String("reason", out.Reason.String()),
			)
			return OutcomeInternal
		}
	}
	if out.Verified {
		if out.Reason == management.ReasonAlreadyVerified {
			return OutcomeAlreadyVerified
		}
		return OutcomeVerified
	}
	// Verified=false with no error: legacy path the use-case does not
	// currently take. Treat as internal so the cap eventually fires.
	return OutcomeInternal
}

// scheduleBackoff bumps the entry's nextTryAt by the next exponential
// step. ssrfHold uses MaxBackoff directly so an SSRF-blocked domain
// only retries once per MaxBackoff window — the user must fix DNS.
func (w *Worker) scheduleBackoff(id uuid.UUID, now time.Time, ssrfHold bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	entry := w.progress[id]
	if entry == nil {
		return
	}
	delay := w.computeDelay(entry.attempts, ssrfHold)
	entry.nextTryAt = now.Add(delay)
}

// computeDelay returns the wait between retries given the current
// attempt count. The first retry uses InitialBackoff; each subsequent
// retry multiplies by BackoffFactor, capped at MaxBackoff.
func (w *Worker) computeDelay(attempts int, ssrfHold bool) time.Duration {
	if ssrfHold {
		return w.maxBackoff
	}
	d := w.initialBackoff
	for i := 1; i < attempts; i++ {
		d = time.Duration(float64(d) * w.backoffFactor)
		if d >= w.maxBackoff {
			return w.maxBackoff
		}
	}
	return d
}

func (w *Worker) bumpAttempt(id uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.progress[id]; entry != nil {
		entry.attempts++
	}
}

func (w *Worker) attemptsFor(id uuid.UUID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.progress[id]; entry != nil {
		return entry.attempts
	}
	return 0
}

func (w *Worker) dropProgress(id uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.progress, id)
}

// pruneInactive removes bookkeeping entries for domains that no longer
// appear in the pending list. Keeps the in-memory map bounded over a
// long-running process (users delete domains, verifications succeed
// off-band, etc.).
func (w *Worker) pruneInactive(active []management.Domain) {
	keep := make(map[uuid.UUID]struct{}, len(active))
	for _, d := range active {
		keep[d.ID] = struct{}{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.progress {
		if _, ok := keep[id]; !ok {
			delete(w.progress, id)
		}
	}
}

// giveUp persists the failed state, logs an audit event, and drops the
// row from the in-memory tracker. Errors from MarkFailed are non-fatal
// — the next tick will re-list the row and either retry the give-up or
// observe that another writer already flipped failed_at.
func (w *Worker) giveUp(ctx context.Context, d management.Domain, attempts int, now time.Time, logger *slog.Logger) {
	if _, err := w.store.MarkFailed(ctx, d.ID, now, FailureReasonCapExceeded); err != nil {
		logger.Error("customdomain_verifier: mark failed errored",
			slog.String("err", err.Error()),
			slog.Int("attempts", attempts),
		)
		return
	}
	w.dropProgress(d.ID)
	w.metrics.incGiveUp()
	if w.audit != nil {
		w.audit.LogVerifierGiveUp(ctx, GiveUpEvent{
			TenantID: d.TenantID,
			DomainID: d.ID,
			Host:     d.Host,
			Reason:   FailureReasonCapExceeded,
			Attempts: attempts,
			At:       now,
		})
	}
	logger.Warn("customdomain_verifier: marked failed (cap exceeded)",
		slog.Int("attempts", attempts),
	)
}
