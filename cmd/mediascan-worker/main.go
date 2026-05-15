// mediascan-worker is the standalone process described in SIN-62804
// (F2-05c) + SIN-62805 (F2-05d): it subscribes to media.scan.requested,
// dispatches each delivery to the worker handler, and exits cleanly on
// SIGTERM. On infected verdicts the handler moves the blob into the
// quarantine bucket via the MinIO adapter and pages #security via the
// Slack adapter — both wired here at the entrypoint so the worker
// package stays vendor-SDK-free.
//
// Configuration is read from the environment to keep secrets out of
// flags and config files (12-factor):
//
//	NATS_URL                  mandatory, e.g. tls://nats.example:4222
//	POSTGRES_DSN              mandatory, pgxpool DSN for app_runtime
//	CLAMD_ADDR                mandatory, host:port of clamd
//	WORKER_CONCURRENCY        optional, default 4 (see comments below)
//	MEDIA_STREAM_NAME         optional, default "MEDIA_SCAN"
//	MEDIA_DURABLE_NAME        optional, default "mediascan-worker"
//	MEDIA_QUEUE_NAME          optional, default "mediascan-workers"
//	BLOB_BASE_DIR             optional, local fs root for dev — used
//	                          ONLY when MINIO_ENDPOINT is unset (e.g.
//	                          unit / smoke runs without MinIO).
//	MINIO_ENDPOINT            mandatory in production; absent enables
//	                          the local-fs BlobReader for dev.
//	MINIO_REGION              optional, default "us-east-1".
//	MINIO_QUARANTINE_SOURCE   mandatory when MINIO_ENDPOINT is set,
//	                          runtime media bucket (e.g. "media").
//	MINIO_QUARANTINE_DEST     mandatory when MINIO_ENDPOINT is set,
//	                          quarantine bucket (e.g. "media-quarantine").
//	MINIO_ACCESS_KEY_ID       mandatory when MINIO_ENDPOINT is set.
//	MINIO_SECRET_ACCESS_KEY   mandatory when MINIO_ENDPOINT is set.
//	MINIO_SESSION_TOKEN       optional; set when credentials came from
//	                          MinIO STS assume-role (production).
//	SLACK_WEBHOOK_URL         optional; when set, infected verdicts
//	                          page the #security channel. Absent keeps
//	                          the alerter nil (worker skips notify).
//
// NATS auth + TLS hardening ([SIN-62815]). Production deploys MUST set
// one of these auth knobs (pick exactly one):
//
//	NATS_CREDS_FILE     path to chained .creds JWT — preferred for
//	                    production (rotatable on disk, scoped subject).
//	NATS_NKEY_FILE      path to NKey seed file (no JWT).
//	NATS_TOKEN          shared-secret bearer token (legacy / dev).
//
// For TLS / mTLS:
//
//	NATS_TLS_CA         PEM bundle path; required when NATS_URL is
//	                    tls:// or wss:// (unless NATS_INSECURE=1).
//	NATS_TLS_CERT       optional client cert for mTLS (paired w/ KEY).
//	NATS_TLS_KEY        optional client key for mTLS  (paired w/ CERT).
//
// NATS_INSECURE=1 opts the process out of the secure-by-default posture
// (plaintext + no auth). Only acceptable for in-cluster dev rigs that
// ride a private network; pre-deploy review blocks it for prod.
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

	slackadapter "github.com/pericles-luz/crm/internal/adapter/alert/slack"
	pgmessagemedia "github.com/pericles-luz/crm/internal/adapter/db/postgres/messagemedia"
	clamavadapter "github.com/pericles-luz/crm/internal/adapter/media/clamav"
	minioadapter "github.com/pericles-luz/crm/internal/adapter/media/minio"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/media/alert"
	"github.com/pericles-luz/crm/internal/media/quarantine"
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

	blobs, err := buildBlobReader(cfg)
	if err != nil {
		return fmt.Errorf("blob reader: %w", err)
	}

	scannerAdapter, err := clamavadapter.New(clamavadapter.Config{
		Addr: cfg.clamdAddr,
	}, blobs)
	if err != nil {
		return fmt.Errorf("clamav.New: %w", err)
	}

	natsAdapter, err := natsadapter.Connect(rootCtx, natsadapter.SDKConfig{
		URL:           cfg.natsURL,
		Name:          "crm-mediascan-worker",
		MaxReconnects: -1,
		Token:         cfg.natsToken,
		NKeyFile:      cfg.natsNKeyFile,
		CredsFile:     cfg.natsCredsFile,
		TLSCAFile:     cfg.natsTLSCAFile,
		TLSCertFile:   cfg.natsTLSCertFile,
		TLSKeyFile:    cfg.natsTLSKeyFile,
		Insecure:      cfg.natsInsecure,
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

	// Defense-in-depth wiring ([SIN-62805] F2-05d). The MinIO Quarantiner
	// is mandatory when MINIO_ENDPOINT is set — its absence means the
	// worker would silently re-serve infected blobs. The Slack alerter
	// is optional: absence means infected verdicts page only via Loki
	// (the worker still logs at ERROR level), which is acceptable for
	// non-production environments.
	if cfg.minioEndpoint != "" {
		q, err := minioadapter.New(minioadapter.Config{
			Endpoint:          cfg.minioEndpoint,
			Region:            cfg.minioRegion,
			SourceBucket:      cfg.minioSource,
			DestinationBucket: cfg.minioDest,
			AccessKeyID:       cfg.minioAccessKey,
			SecretAccessKey:   cfg.minioSecretKey,
			SessionToken:      cfg.minioSessionToken,
		})
		if err != nil {
			return fmt.Errorf("minio.New (quarantine): %w", err)
		}
		handler.Quarantiner = quarantine.Quarantiner(q)
	}
	if cfg.slackWebhookURL != "" {
		a, err := slackadapter.NewMediaAlerter(slackadapter.Config{
			WebhookURL: cfg.slackWebhookURL,
		})
		if err != nil {
			return fmt.Errorf("slack.NewMediaAlerter: %w", err)
		}
		handler.Alerter = alert.Alerter(a)
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

	// Log the security posture so an operator can audit the deploy
	// without grepping env. Paths only; never the secret material.
	logger.Info("mediascan-worker ready",
		"nats", cfg.natsURL,
		"stream", cfg.streamName,
		"queue", cfg.queueName,
		"concurrency", cfg.concurrency,
		"auth", natsAuthMode(cfg),
		"tls_ca", cfg.natsTLSCAFile,
		"mtls", cfg.natsTLSCertFile != "" && cfg.natsTLSKeyFile != "",
		"insecure", cfg.natsInsecure,
		"quarantiner", handler.Quarantiner != nil,
		"alerter", handler.Alerter != nil,
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

	// NATS auth + TLS — see package doc for the env knobs ([SIN-62815]).
	natsToken       string
	natsNKeyFile    string
	natsCredsFile   string
	natsTLSCAFile   string
	natsTLSCertFile string
	natsTLSKeyFile  string
	natsInsecure    bool

	// MinIO/S3 defense-in-depth wiring. When minioEndpoint is empty,
	// the local-fs BlobReader is used and the Quarantiner stays nil
	// (worker logs infected verdicts but does not move the blob).
	minioEndpoint     string
	minioRegion       string
	minioSource       string
	minioDest         string
	minioAccessKey    string
	minioSecretKey    string
	minioSessionToken string

	// Slack #security webhook (optional). When empty the Alerter stays
	// nil and infected verdicts are visible only in worker logs.
	slackWebhookURL string
}

func loadConfig() (config, error) {
	c := config{
		natsURL:           os.Getenv("NATS_URL"),
		postgresDSN:       os.Getenv("POSTGRES_DSN"),
		clamdAddr:         os.Getenv("CLAMD_ADDR"),
		streamName:        envOr("MEDIA_STREAM_NAME", "MEDIA_SCAN"),
		durableName:       envOr("MEDIA_DURABLE_NAME", "mediascan-worker"),
		queueName:         envOr("MEDIA_QUEUE_NAME", "mediascan-workers"),
		blobBaseDir:       os.Getenv("BLOB_BASE_DIR"),
		natsToken:         os.Getenv("NATS_TOKEN"),
		natsNKeyFile:      os.Getenv("NATS_NKEY_FILE"),
		natsCredsFile:     os.Getenv("NATS_CREDS_FILE"),
		natsTLSCAFile:     os.Getenv("NATS_TLS_CA"),
		natsTLSCertFile:   os.Getenv("NATS_TLS_CERT"),
		natsTLSKeyFile:    os.Getenv("NATS_TLS_KEY"),
		natsInsecure:      envBool("NATS_INSECURE"),
		minioEndpoint:     os.Getenv("MINIO_ENDPOINT"),
		minioRegion:       envOr("MINIO_REGION", "us-east-1"),
		minioSource:       os.Getenv("MINIO_QUARANTINE_SOURCE"),
		minioDest:         os.Getenv("MINIO_QUARANTINE_DEST"),
		minioAccessKey:    os.Getenv("MINIO_ACCESS_KEY_ID"),
		minioSecretKey:    os.Getenv("MINIO_SECRET_ACCESS_KEY"),
		minioSessionToken: os.Getenv("MINIO_SESSION_TOKEN"),
		slackWebhookURL:   os.Getenv("SLACK_WEBHOOK_URL"),
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
	if c.minioEndpoint != "" {
		// MINIO_ENDPOINT presence promotes the rest to required —
		// half-configured S3 would be a worse failure mode than no S3.
		if c.minioSource == "" {
			missing = append(missing, "MINIO_QUARANTINE_SOURCE")
		}
		if c.minioDest == "" {
			missing = append(missing, "MINIO_QUARANTINE_DEST")
		}
		if c.minioAccessKey == "" {
			missing = append(missing, "MINIO_ACCESS_KEY_ID")
		}
		if c.minioSecretKey == "" {
			missing = append(missing, "MINIO_SECRET_ACCESS_KEY")
		}
	}
	// Note: the "BLOB_BASE_DIR or MINIO_ENDPOINT+credentials" requirement
	// is enforced by buildBlobReader at startup (run()), not here, so
	// loadConfig stays a pure env-parsing step and tests that only
	// exercise NATS/concurrency knobs do not have to set blob env.
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %v", missing)
	}

	// Cross-field NATS security validation. SDKConfig.validate() would
	// catch most of this on Connect, but failing earlier with a config-
	// level message makes the deploy error clearer to the operator and
	// keeps the error wording aimed at env knobs rather than struct
	// fields.
	if err := validateNATSSecurity(c); err != nil {
		return c, err
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

// validateNATSSecurity rejects deploy mistakes at startup before any
// socket is opened. Logging deliberately surfaces paths (so the
// operator can fix the env) but never file contents.
func validateNATSSecurity(c config) error {
	authCount := 0
	if c.natsToken != "" {
		authCount++
	}
	if c.natsNKeyFile != "" {
		authCount++
	}
	if c.natsCredsFile != "" {
		authCount++
	}
	if authCount > 1 {
		return errors.New("NATS auth: set at most one of NATS_TOKEN, NATS_NKEY_FILE, NATS_CREDS_FILE")
	}

	scheme := strings.ToLower(strings.SplitN(c.natsURL, "://", 2)[0])
	switch scheme {
	case "tls", "wss":
		if c.natsTLSCAFile == "" && !c.natsInsecure {
			return fmt.Errorf("NATS_URL is %s:// but NATS_TLS_CA is empty (set NATS_TLS_CA=/path/to/ca.pem or NATS_INSECURE=1 to bypass)", scheme)
		}
	case "nats", "ws":
		if !c.natsInsecure {
			return errors.New("NATS_URL is plaintext; set a tls:// URL with NATS_TLS_CA, or NATS_INSECURE=1 to acknowledge the insecure transport")
		}
	}

	// mTLS pairing — easier to catch here than at Connect time.
	if (c.natsTLSCertFile != "") != (c.natsTLSKeyFile != "") {
		return errors.New("NATS mTLS: NATS_TLS_CERT and NATS_TLS_KEY must be set together")
	}

	return nil
}

// natsAuthMode reports which auth knob is wired, never the secret
// itself. Used for the startup log line.
func natsAuthMode(c config) string {
	switch {
	case c.natsCredsFile != "":
		return "creds-file"
	case c.natsNKeyFile != "":
		return "nkey-file"
	case c.natsToken != "":
		return "token"
	default:
		return "none"
	}
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// buildBlobReader returns the BlobReader the ClamAV adapter uses to
// fetch the runtime blob. Production: MinIO Reader against the runtime
// `media` bucket. Dev/smoke without MinIO: local-fs reader rooted at
// BLOB_BASE_DIR. Exactly one of MINIO_ENDPOINT or BLOB_BASE_DIR must be
// configured; the worker fails fast at startup otherwise so a deploy
// with both unset cannot silently re-serve infected blobs.
func buildBlobReader(cfg config) (clamavadapter.BlobReader, error) {
	if cfg.minioEndpoint != "" {
		return minioadapter.NewReader(minioadapter.ReaderConfig{
			Endpoint:        cfg.minioEndpoint,
			Region:          cfg.minioRegion,
			Bucket:          cfg.minioSource,
			AccessKeyID:     cfg.minioAccessKey,
			SecretAccessKey: cfg.minioSecretKey,
			SessionToken:    cfg.minioSessionToken,
		})
	}
	if cfg.blobBaseDir == "" {
		return nil, errors.New("blob reader: set BLOB_BASE_DIR (dev) or MINIO_ENDPOINT + credentials (prod)")
	}
	return &localBlobs{root: cfg.blobBaseDir}, nil
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

// localBlobs is the dev BlobReader for ClamAV: reads the media blob
// from a local directory. Production swaps it for the MinIO Reader
// above; the indirection lives in main so neither the worker nor the
// ClamAV adapter depends on a specific blob backend.
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
