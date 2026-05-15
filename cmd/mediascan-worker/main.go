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
//	MINIO_ACCESS_KEY_ID       mandatory when MINIO_ENDPOINT is set AND
//	                          MINIO_CREDS_FILE is unset; otherwise the
//	                          adapter pulls the triple from the file.
//	MINIO_SECRET_ACCESS_KEY   same conditional as MINIO_ACCESS_KEY_ID.
//	MINIO_SESSION_TOKEN       optional; set when credentials came from
//	                          MinIO STS assume-role (production). Ignored
//	                          when MINIO_CREDS_FILE is set.
//	MINIO_CREDS_FILE          optional; path to a JSON file containing
//	                          {accessKey, secretKey, sessionToken}.
//	                          Production STS rotation [SIN-62819]: a
//	                          sidecar rewrites this file every
//	                          MINIO_CREDS_REFRESH minutes; the adapter
//	                          re-reads it inside RotatingProvider so no
//	                          long-lived creds live in the container.
//	MINIO_CREDS_REFRESH       optional Go duration, default 50m. Cache
//	                          TTL for credentials read from
//	                          MINIO_CREDS_FILE — matches a 1h STS triple
//	                          with a 10-minute safety margin.
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
	"github.com/pericles-luz/crm/internal/media/scanner"
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

	return runWithStore(rootCtx, cfg, store, logger)
}

// runWithStore is the testable interior of run() — it does everything
// except dial Postgres + build the Store, which require live infra.
// Tests drive runWithStore with an in-memory MessageMediaStore fake and
// drive each error branch directly (bad MinIO creds, bad NATS URL,
// invalid clamd addr, invalid Slack webhook) without standing up a
// real Postgres / NATS / MinIO / Slack rig.
func runWithStore(ctx context.Context, cfg config, store scanner.MessageMediaStore, logger *slog.Logger) error {
	sharedMinioProvider, err := openMinioProvider(cfg)
	if err != nil {
		return fmt.Errorf("minio credentials: %w", err)
	}

	blobs, err := buildBlobReaderWithProvider(cfg, sharedMinioProvider)
	if err != nil {
		return fmt.Errorf("blob reader: %w", err)
	}

	scannerAdapter, err := openScanner(cfg, blobs)
	if err != nil {
		return fmt.Errorf("clamav.New: %w", err)
	}

	natsAdapter, err := natsadapter.Connect(ctx, buildNATSConfig(cfg))
	if err != nil {
		return fmt.Errorf("nats.Connect: %w", err)
	}
	defer natsAdapter.Close()

	quarantiner, err := buildQuarantiner(cfg, sharedMinioProvider)
	if err != nil {
		return fmt.Errorf("minio.New (quarantine): %w", err)
	}
	alerter, err := buildAlerter(cfg)
	if err != nil {
		return fmt.Errorf("slack.NewMediaAlerter: %w", err)
	}

	return Wire(ctx, Deps{
		Cfg:         cfg,
		Logger:      logger,
		Scanner:     scannerAdapter,
		Store:       store,
		NATS:        &natsAdapterShim{a: natsAdapter},
		Publisher:   &publisherShim{a: natsAdapter},
		Quarantiner: quarantiner,
		Alerter:     alerter,
	})
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

	// minioCredsFile points to a JSON triple rotated by a sidecar; when
	// non-empty, it overrides the static AccessKey/SecretKey/SessionToken
	// fields. minioCredsRefresh is the cache TTL applied to that file —
	// see RotatingProvider in the minio adapter ([SIN-62819]).
	minioCredsFile    string
	minioCredsRefresh time.Duration

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
		minioCredsFile:    os.Getenv("MINIO_CREDS_FILE"),
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
		// MINIO_CREDS_FILE overrides the static AK/SK envs: a sidecar
		// rotates the on-disk JSON every MINIO_CREDS_REFRESH. Either path
		// must produce a non-empty triple — half-configured creds are
		// rejected for the same reason as half-configured S3.
		if c.minioCredsFile == "" {
			if c.minioAccessKey == "" {
				missing = append(missing, "MINIO_ACCESS_KEY_ID")
			}
			if c.minioSecretKey == "" {
				missing = append(missing, "MINIO_SECRET_ACCESS_KEY")
			}
		} else if c.minioAccessKey != "" || c.minioSecretKey != "" || c.minioSessionToken != "" {
			return c, errors.New("MINIO_CREDS_FILE overrides MINIO_ACCESS_KEY_ID / MINIO_SECRET_ACCESS_KEY / MINIO_SESSION_TOKEN — unset the static envs when using the rotating file")
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

	c.minioCredsRefresh = 50 * time.Minute
	if v := os.Getenv("MINIO_CREDS_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("MINIO_CREDS_REFRESH %q: must be positive Go duration (e.g. 50m)", v)
		}
		c.minioCredsRefresh = d
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

// minioCredsMode reports the credentials source for the MinIO adapter
// without logging the secret material. "none" means MINIO_ENDPOINT was
// not set and the worker is running against the local-fs blob reader.
func minioCredsMode(c config) string {
	switch {
	case c.minioEndpoint == "":
		return "none"
	case c.minioCredsFile != "":
		return "rotating-file"
	default:
		return "static-env"
	}
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

// openMinioProvider returns the shared MinIO CredentialsProvider when
// MINIO_ENDPOINT is set, nil otherwise. The Reader and the Quarantiner
// MUST consume the same returned value so STS refresh ([SIN-62819]) is
// hit at most once per cfg.minioCredsRefresh.
func openMinioProvider(cfg config) (minioadapter.CredentialsProvider, error) {
	if cfg.minioEndpoint == "" {
		return nil, nil
	}
	return buildCredentialsProvider(cfg)
}

// openScanner constructs the ClamAV MediaScanner from cfg.clamdAddr
// plus the resolved blob reader. Thin wrap kept here so run() does not
// need to know the adapter's Config shape; tests can drive the
// error-path (empty Addr / nil blobs) without standing up clamd.
func openScanner(cfg config, blobs clamavadapter.BlobReader) (scanner.MediaScanner, error) {
	return clamavadapter.New(clamavadapter.Config{Addr: cfg.clamdAddr}, blobs)
}

// buildNATSConfig translates the parsed env config into the SDK-shaped
// SDKConfig the natsadapter consumes. Pure data transform — no I/O —
// so it can be exercised by unit tests without dialing NATS.
func buildNATSConfig(cfg config) natsadapter.SDKConfig {
	return natsadapter.SDKConfig{
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
	}
}

// buildQuarantiner returns the MinIO-backed Quarantiner when
// MINIO_ENDPOINT is set, otherwise nil. The provider MUST be the same
// CredentialsProvider passed to buildBlobReaderWithProvider so the
// Reader and the Quarantiner share the same RotatingProvider cache
// ([SIN-62819]).
//
// Defense-in-depth wiring ([SIN-62805] F2-05d). The MinIO Quarantiner
// is mandatory when MINIO_ENDPOINT is set — its absence means the
// worker would silently re-serve infected blobs.
func buildQuarantiner(cfg config, provider minioadapter.CredentialsProvider) (quarantine.Quarantiner, error) {
	if cfg.minioEndpoint == "" {
		return nil, nil
	}
	q, err := minioadapter.New(minioadapter.Config{
		Endpoint:            cfg.minioEndpoint,
		Region:              cfg.minioRegion,
		SourceBucket:        cfg.minioSource,
		DestinationBucket:   cfg.minioDest,
		CredentialsProvider: provider,
	})
	if err != nil {
		return nil, err
	}
	return quarantine.Quarantiner(q), nil
}

// buildAlerter returns the Slack-backed Alerter when SLACK_WEBHOOK_URL
// is set, otherwise nil. The Slack alerter is optional: absence means
// infected verdicts page only via Loki (the worker still logs at ERROR
// level), which is acceptable for non-production environments.
func buildAlerter(cfg config) (alert.Alerter, error) {
	if cfg.slackWebhookURL == "" {
		return nil, nil
	}
	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{
		WebhookURL: cfg.slackWebhookURL,
	})
	if err != nil {
		return nil, err
	}
	return alert.Alerter(a), nil
}

// buildBlobReader returns the BlobReader the ClamAV adapter uses to
// fetch the runtime blob. Production: MinIO Reader against the runtime
// `media` bucket. Dev/smoke without MinIO: local-fs reader rooted at
// BLOB_BASE_DIR. Exactly one of MINIO_ENDPOINT or BLOB_BASE_DIR must be
// configured; the worker fails fast at startup otherwise so a deploy
// with both unset cannot silently re-serve infected blobs.
//
// This helper builds its own RotatingProvider for the MinIO branch.
// Production wiring (run()) uses buildBlobReaderWithProvider so the
// Reader and the Quarantiner share a single provider; this entrypoint
// stays for tests that exercise the dev/local-fs branch and the
// MINIO_ENDPOINT-without-BLOB_BASE_DIR error path.
func buildBlobReader(cfg config) (clamavadapter.BlobReader, error) {
	var provider minioadapter.CredentialsProvider
	if cfg.minioEndpoint != "" {
		p, err := buildCredentialsProvider(cfg)
		if err != nil {
			return nil, fmt.Errorf("blob reader credentials: %w", err)
		}
		provider = p
	}
	return buildBlobReaderWithProvider(cfg, provider)
}

// buildBlobReaderWithProvider is the production-side BlobReader
// builder. When MINIO_ENDPOINT is set the caller MUST pass the same
// CredentialsProvider it wires into the Quarantiner — that is the
// shared-cache invariant ([SIN-62819]) the package comment promises.
// Passing a nil provider while MINIO_ENDPOINT is set is rejected so a
// caller cannot silently bypass the cache by forgetting the share.
func buildBlobReaderWithProvider(cfg config, provider minioadapter.CredentialsProvider) (clamavadapter.BlobReader, error) {
	if cfg.minioEndpoint != "" {
		if provider == nil {
			return nil, errors.New("blob reader: MINIO_ENDPOINT set but credentials provider is nil")
		}
		return minioadapter.NewReader(minioadapter.ReaderConfig{
			Endpoint:            cfg.minioEndpoint,
			Region:              cfg.minioRegion,
			Bucket:              cfg.minioSource,
			CredentialsProvider: provider,
		})
	}
	if cfg.blobBaseDir == "" {
		return nil, errors.New("blob reader: set BLOB_BASE_DIR (dev) or MINIO_ENDPOINT + credentials (prod)")
	}
	return &localBlobs{root: cfg.blobBaseDir}, nil
}

// buildCredentialsProvider returns the CredentialsProvider used by both
// the Quarantiner and the Reader. When MINIO_CREDS_FILE is set the
// returned provider re-reads the file every cfg.minioCredsRefresh — the
// production path against an STS sidecar. Without the file, a static
// triple is returned (dev / smoke) so the same wiring works in both
// environments.
func buildCredentialsProvider(cfg config) (minioadapter.CredentialsProvider, error) {
	if cfg.minioCredsFile != "" {
		refresh, err := minioadapter.NewFileRefresher(cfg.minioCredsFile)
		if err != nil {
			return nil, err
		}
		return minioadapter.NewRotatingProvider(minioadapter.RotatingProviderConfig{
			Refresh:  refresh,
			Interval: cfg.minioCredsRefresh,
		})
	}
	return minioadapter.StaticProvider(minioadapter.Credentials{
		AccessKeyID:     cfg.minioAccessKey,
		SecretAccessKey: cfg.minioSecretKey,
		SessionToken:    cfg.minioSessionToken,
	})
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
