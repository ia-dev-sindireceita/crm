// mediascan-worker is the standalone process described in SIN-62804
// (F2-05c): it subscribes to media.scan.requested, dispatches each
// delivery to the worker handler, and exits cleanly on SIGTERM.
//
// Configuration is read from the environment to keep secrets out of
// flags and config files (12-factor):
//
//	NATS_URL            mandatory, e.g. nats://nats:4222
//	POSTGRES_DSN        mandatory, pgxpool DSN for app_runtime
//	CLAMD_ADDR          mandatory, host:port of clamd
//	WORKER_CONCURRENCY  optional, default 4 (see comments below)
//	MEDIA_STREAM_NAME   optional, default "MEDIA_SCAN"
//	MEDIA_DURABLE_NAME  optional, default "mediascan-worker"
//	MEDIA_QUEUE_NAME    optional, default "mediascan-workers"
//	BLOB_BASE_DIR       optional, local fs root used when no S3 is
//	                    wired (Fase 1 development); the production
//	                    blob reader is the S3/MinIO adapter to be
//	                    introduced alongside SIN-62805.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgmessagemedia "github.com/pericles-luz/crm/internal/adapter/db/postgres/messagemedia"
	clamavadapter "github.com/pericles-luz/crm/internal/adapter/media/clamav"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/media/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("mediascan-worker exited", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(rootCtx, cfg.postgresDSN)
	if err != nil {
		return fmt.Errorf("pgxpool.New: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(rootCtx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}

	store, err := pgmessagemedia.New(pool)
	if err != nil {
		return fmt.Errorf("messagemedia.New: %w", err)
	}

	scannerAdapter, err := clamavadapter.New(clamavadapter.Config{
		Addr: cfg.clamdAddr,
	}, &localBlobs{root: cfg.blobBaseDir})
	if err != nil {
		return fmt.Errorf("clamav.New: %w", err)
	}

	natsAdapter, err := natsadapter.Connect(rootCtx, natsadapter.SDKConfig{
		URL:           cfg.natsURL,
		Name:          "crm-mediascan-worker",
		MaxReconnects: -1,
	})
	if err != nil {
		return fmt.Errorf("nats.Connect: %w", err)
	}
	defer natsAdapter.Close()

	if err := natsAdapter.EnsureStream(cfg.streamName, []string{
		worker.SubjectRequested,
		worker.SubjectCompleted,
	}); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}

	handler, err := worker.New(scannerAdapter, store, &publisherShim{a: natsAdapter}, logger)
	if err != nil {
		return fmt.Errorf("worker.New: %w", err)
	}

	// Concurrency is enforced via a buffered semaphore so the
	// QueueSubscribe callback does not spawn unbounded goroutines on
	// burst. AckWait is set to the slowest scan we expect to
	// tolerate (default 30s for ClamAV INSTREAM on multi-MB blobs);
	// graceful shutdown waits for the semaphore to drain.
	sem := make(chan struct{}, cfg.concurrency)
	done := make(chan struct{})

	sub, err := natsAdapter.Subscribe(rootCtx, worker.SubjectRequested,
		cfg.queueName, cfg.durableName, 30*time.Second,
		func(ctx context.Context, d *natsadapter.Delivery) error {
			sem <- struct{}{}
			defer func() { <-sem }()
			return handler.Handle(ctx, d)
		},
	)
	if err != nil {
		return fmt.Errorf("nats subscribe: %w", err)
	}

	logger.Info("mediascan-worker ready",
		"nats", cfg.natsURL,
		"stream", cfg.streamName,
		"queue", cfg.queueName,
		"concurrency", cfg.concurrency,
	)

	<-rootCtx.Done()
	close(done)

	// Stop accepting new deliveries; in-flight ones get up to
	// AckWait to drain. Drain the NATS conn so the broker sees us
	// leave cleanly.
	logger.Info("mediascan-worker shutting down")
	_ = sub.Drain()
	if err := natsAdapter.Drain(); err != nil {
		logger.Warn("nats drain", "err", err.Error())
	}
	return nil
}

// config is the parsed env. Required fields are kept private to force
// callers through loadConfig (which validates).
type config struct {
	natsURL     string
	postgresDSN string
	clamdAddr   string
	concurrency int
	streamName  string
	durableName string
	queueName   string
	blobBaseDir string
}

func loadConfig() (config, error) {
	c := config{
		natsURL:     os.Getenv("NATS_URL"),
		postgresDSN: os.Getenv("POSTGRES_DSN"),
		clamdAddr:   os.Getenv("CLAMD_ADDR"),
		streamName:  envOr("MEDIA_STREAM_NAME", "MEDIA_SCAN"),
		durableName: envOr("MEDIA_DURABLE_NAME", "mediascan-worker"),
		queueName:   envOr("MEDIA_QUEUE_NAME", "mediascan-workers"),
		blobBaseDir: os.Getenv("BLOB_BASE_DIR"),
	}
	missing := []string{}
	if c.natsURL == "" {
		missing = append(missing, "NATS_URL")
	}
	if c.postgresDSN == "" {
		missing = append(missing, "POSTGRES_DSN")
	}
	if c.clamdAddr == "" {
		missing = append(missing, "CLAMD_ADDR")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %v", missing)
	}
	c.concurrency = 4
	if v := os.Getenv("WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("WORKER_CONCURRENCY %q: must be positive integer", v)
		}
		c.concurrency = n
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// publisherShim adapts SDKAdapter.Publish to the worker.Publisher
// interface — the worker is intentionally unaware of NATS so this
// glue lives at the wire-up boundary.
type publisherShim struct {
	a *natsadapter.SDKAdapter
}

func (p *publisherShim) Publish(ctx context.Context, subject string, body []byte) error {
	return p.a.Publish(ctx, subject, body)
}

// localBlobs is the Fase 1 BlobReader for ClamAV: reads the media
// blob from a local directory. Production (SIN-62805) swaps this for
// an S3/MinIO adapter; the indirection lives in main so neither the
// worker nor the ClamAV adapter depends on a specific blob backend.
type localBlobs struct{ root string }

func (l *localBlobs) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if l.root == "" {
		return nil, errors.New("BLOB_BASE_DIR is required to open local blobs")
	}
	// Reject escapes (keys starting with .. or absolute paths).
	clean := filepath.Clean(key)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("localBlobs: refusing to open escape key %q", key)
	}
	return os.Open(filepath.Join(l.root, clean))
}
