// Package lgpd_retention is the daily worker that finalises LGPD
// erasure requests once their fiscal-retention window has elapsed.
// AC #4 of SIN-63186: re-uses the cron/tick pattern from
// internal/worker/customdomain_verifier so the operational shape is
// already familiar.
//
// The worker is intentionally small: one tick per Interval, fetch up
// to BatchSize ready rows, purge each through PurgeRepository, mark
// the request completed (or failed). No queues, no in-memory state
// between ticks — restartable at any time.
package lgpd_retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pericles-luz/crm/internal/lgpd"
)

// Defaults the worker falls back to when Config leaves a field zero.
const (
	// DefaultInterval is the inter-tick delay. AC #4: "daily" — but a
	// shorter cadence avoids holding completed-but-pending rows for an
	// extra 24h after a backlog clears. 1h keeps the row through-put
	// near real-time while still being well under any DB cost concern.
	DefaultInterval = 1 * time.Hour

	// DefaultBatchSize caps how many rows a single tick processes. A
	// large backlog after an outage still finishes inside a normal
	// maintenance window without flooding the audit ledger.
	DefaultBatchSize = 100
)

// Config groups Worker dependencies. New returns an error for any
// missing required field and fills defaults for the rest.
type Config struct {
	// Deletions is the lgpd_deletion_request port. Required.
	Deletions lgpd.DeletionRepository
	// Purge is the contact-anonymisation port. Required.
	Purge lgpd.PurgeRepository
	// Logger is the structured logger. Defaults to slog.Default.
	Logger *slog.Logger

	// Interval is the inter-tick delay. Defaults to DefaultInterval.
	Interval time.Duration
	// BatchSize is the per-tick row cap. Defaults to DefaultBatchSize.
	BatchSize int

	// Clock returns "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Worker holds the dependencies for a running retention sweep.
// Construct once with New, then call Run from the main goroutine.
type Worker struct {
	deletions lgpd.DeletionRepository
	purge     lgpd.PurgeRepository
	logger    *slog.Logger
	interval  time.Duration
	batchSize int
	now       func() time.Time
}

// New constructs a Worker from cfg.
func New(cfg Config) (*Worker, error) {
	if cfg.Deletions == nil {
		return nil, errors.New("lgpd_retention: Deletions is required")
	}
	if cfg.Purge == nil {
		return nil, errors.New("lgpd_retention: Purge is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		deletions: cfg.Deletions,
		purge:     cfg.Purge,
		logger:    cfg.Logger,
		interval:  cfg.Interval,
		batchSize: cfg.BatchSize,
		now:       cfg.Clock,
	}, nil
}

// Run blocks until ctx is cancelled. It runs Tick once immediately so
// a restarted worker drains any backlog without waiting a full
// Interval — important after a deploy outage.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("lgpd_retention: ready", "interval", w.interval.String(), "batch_size", w.batchSize)
	if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("lgpd_retention: initial tick failed", "err", err.Error())
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("lgpd_retention: shutting down")
			return nil
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Warn("lgpd_retention: tick failed", "err", err.Error())
			}
		}
	}
}

// Tick runs one purge pass. Exported so the cmd binary's smoke test
// and the integration tests can drive a deterministic sweep.
func (w *Worker) Tick(ctx context.Context) error {
	at := w.now()
	ready, err := w.deletions.ListReady(ctx, at, w.batchSize)
	if err != nil {
		return fmt.Errorf("list ready: %w", err)
	}
	for _, req := range ready {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.purge.PurgeContact(ctx, req.TenantID, req.ContactID); err != nil {
			w.logger.Warn("lgpd_retention: purge failed",
				"deletion_request_id", req.ID.String(),
				"tenant_id", req.TenantID.String(),
				"contact_id", req.ContactID.String(),
				"err", err.Error())
			if markErr := w.deletions.MarkFailed(ctx, req.ID, w.now()); markErr != nil {
				w.logger.Warn("lgpd_retention: mark failed",
					"deletion_request_id", req.ID.String(), "err", markErr.Error())
			}
			continue
		}
		if err := w.deletions.MarkCompleted(ctx, req.ID, w.now()); err != nil {
			w.logger.Warn("lgpd_retention: mark completed failed",
				"deletion_request_id", req.ID.String(), "err", err.Error())
			continue
		}
		w.logger.Info("lgpd_retention: purged",
			"deletion_request_id", req.ID.String(),
			"tenant_id", req.TenantID.String(),
			"contact_id", req.ContactID.String())
	}
	return nil
}
