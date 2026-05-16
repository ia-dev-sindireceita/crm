// Package main is the CRM HTTP server entrypoint (SIN-62208 Fase 0 PR1).
//
// Two HTTP listeners run concurrently when the SIN-62243 F45 stack is
// wired:
//
//   - Public listener (HTTP_ADDR, default :8080) — routes the public
//     surface (/health today; tenant routes incrementally).
//   - Internal listener (INTERNAL_HTTP_ADDR, default :8081) — exposes
//     ONLY /internal/tls/ask. Bound for docker-internal reachability;
//     compose does NOT publish this port to the host.
//
// The internal listener is wired only when DATABASE_URL and REDIS_URL are
// both present so cmd/server tests / smoke runs without those deps still
// boot the public listener cleanly. When skipped, /internal/tls/ask
// returns 404 from the public listener; this is the F45 acceptance
// criterion "endpoint não responde quando bateado em interface pública".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	crmslog "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	tlsasktransport "github.com/pericles-luz/crm/internal/adapter/transport/http/tlsask"
	"github.com/pericles-luz/crm/internal/customdomain/featureflag"
	"github.com/pericles-luz/crm/internal/customdomain/ratelimit/sliding"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
)

const (
	defaultAddr         = ":8080"
	defaultInternalAddr = ":8081"

	envHTTPAddr     = "HTTP_ADDR"
	envInternalAddr = "INTERNAL_HTTP_ADDR"
	envRedisURL     = "REDIS_URL"

	// PrimaryDomain governs which host the SIN-62331 RedirectHandler
	// wraps the public mux around. Re-uses customdomain_wire's env so
	// both bundles see the same primary apex; default lives in
	// slugreservation_wire.
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(executeAll(ctx, os.Getenv))
}

func execute(ctx context.Context, getenv func(string) string) int {
	addr := defaultAddr
	if v := getenv(envHTTPAddr); v != "" {
		addr = v
	}
	if err := run(ctx, addr); err != nil {
		log.Printf("crm: %v", err)
		return 1
	}
	return 0
}

// executeAll runs the public listener and, when wired, the internal
// /internal/tls/ask listener concurrently. It returns 0 on graceful
// shutdown of both, 1 if either errors.
func executeAll(ctx context.Context, getenv func(string) string) int {
	return executeAllWith(ctx, getenv, defaultDial)
}

func executeAllWith(ctx context.Context, getenv func(string) string, dial dialFn) int {
	publicAddr := defaultAddr
	if v := getenv(envHTTPAddr); v != "" {
		publicAddr = v
	}
	internalAddr := defaultInternalAddr
	if v := getenv(envInternalAddr); v != "" {
		internalAddr = v
	}

	internalHandler, internalCleanup := buildInternalHandlerWith(ctx, getenv, dial)
	defer internalCleanup()

	// SIN-62331 F51 — cookieless static origin (F49). Wired only when
	// STATIC_HTTP_ADDR is set so cmd/server tests / smoke runs without
	// the env stay on the public+internal listener pair.
	staticAddr := getenv(envStaticOriginAddr)
	var staticHandler http.Handler
	if staticAddr != "" {
		h, err := buildMediaServeHandler()
		if err != nil {
			log.Printf("crm: static origin disabled — %v", err)
		} else {
			staticHandler = h
		}
	}

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	collectErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := run(ctx, publicAddr); err != nil {
			collectErr(fmt.Errorf("public listener: %w", err))
		}
	}()
	if internalHandler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runInternal(ctx, internalAddr, internalHandler); err != nil {
				collectErr(fmt.Errorf("internal listener: %w", err))
			}
		}()
	}
	if staticHandler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runStaticOrigin(ctx, staticAddr, staticHandler); err != nil {
				collectErr(fmt.Errorf("static origin listener: %w", err))
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		log.Printf("crm: %v", firstErr)
		return 1
	}
	return 0
}

func run(ctx context.Context, addr string) error {
	return runWith(ctx, addr, os.Getenv, defaultWebhookDial)
}

// runWith is the test-friendly variant of run. The dial seam lets unit
// tests drive the SIN-62300 webhook wiring without Postgres; production
// passes defaultWebhookDial via run().
func runWith(ctx context.Context, addr string, getenv func(string) string, webhookDial webhookDial) error {
	mux := newMux()

	// SIN-62300 webhook intake — registered before the custom-domain
	// catch-all so the routing order is obvious to a reader. Go 1.22+
	// ServeMux already prefers the more-specific `POST /webhooks/...`
	// pattern over `/`, but we register early on principle.
	wh := buildWebhookWiringWithDeps(ctx, getenv, webhookDial)
	if wh != nil {
		defer wh.Cleanup()
		wh.Register(mux)
		log.Printf("crm: webhook intake mounted on public listener")
	}

	// SIN-62731 WhatsApp Cloud-API inbound webhook. Registered after
	// the generic ADR-0075 intake so the more-specific
	// `POST /webhooks/whatsapp` pattern wins over the templated
	// `POST /webhooks/{channel}/{webhook_token}` route (Go 1.22 mux
	// already prefers the more specific pattern).
	wa := buildWhatsAppWiring(ctx, getenv)
	if wa != nil {
		defer wa.Cleanup()
		wa.Register(mux)
	}

	// SIN-62844 Messenger inbound webhook + outbound sender (F2-10 follow-up).
	ms := buildMessengerWiring(ctx, getenv)
	if ms != nil {
		defer ms.Cleanup()
		ms.Register(mux)
	}

	// SIN-62331 F51 — slug reservation wiring. Mount the master
	// override route, the signup + tenant-rename placeholders guarded
	// by RequireSlugAvailable, and the upload pipeline. The redirect
	// handler is applied later as a host-level prefilter so it fires
	// for every request that arrives with `<old>.<primary>` in Host.
	slugWiring := buildSlugReservationWiring(ctx, getenv)
	defer slugWiring.cleanup()
	registerSlugReservationRoutes(mux, slugWiring, getenv)
	registerUploadRoutes(mux)
	log.Printf("crm: slug reservation + upload routes mounted on public listener")

	// SIN-62855 — HTMX identity-split UI (SIN-62799 follow-up). Built
	// before buildIAMHandler so the handler can be threaded into the
	// chi authed group via opts.WebContacts. The cleanup releases this
	// wire's pgxpool independently of the IAM pool.
	webContactsHandler, webContactsCleanup := buildWebContactsHandler(ctx, getenv)
	defer webContactsCleanup()

	// SIN-62862 — HTMX funnel board UI (SIN-62797 follow-up). Same
	// fail-soft pattern as the contacts wire: when DATABASE_URL is
	// unset the handler is nil and the /funnel* routes stay unmounted.
	webFunnelHandler, webFunnelCleanup := buildWebFunnelHandler(ctx, getenv)
	defer webFunnelCleanup()

	// SIN-62354 — HTMX privacy / DPA page (Fase 3, decisão #8 /
	// SIN-62203). Read-only LGPD disclosure; the wire takes no DB
	// dependency today (the active-model lookup falls back to the
	// migration 0098 default until SIN-62351's cascade resolver
	// lands), so the handler is always non-nil here.
	webPrivacyHandler, webPrivacyCleanup := buildWebPrivacyHandler(ctx, getenv)
	defer webPrivacyCleanup()

	// SIN-62527 / SIN-62217 — IAM chi handler (login, logout, hello-tenant,
	// /m/*, metrics). Mounted before the custom-domain catch-all so
	// Go's ServeMux longer-prefix rule keeps IAM routes out of the
	// catch-all handler.
	iamHandler, iamCleanup := buildIAMHandler(ctx, getenv, iamHandlerOpts{
		WebContacts: webContactsHandler,
		WebFunnel:   webFunnelHandler,
		WebPrivacy:  webPrivacyHandler,
	})
	defer iamCleanup()
	if iamHandler != nil {
		for _, pattern := range iamRoutes {
			mux.Handle(pattern, iamHandler)
		}
		log.Printf("crm: IAM routes mounted on public listener")
	}

	// SIN-62334 F53: hard-fail boot when CUSTOM_DOMAIN_UI_ENABLED=1 and
	// REDIS_URL is unset. Returning the error from runWith propagates to
	// main(), which exits non-zero — the orchestrator restarts and the
	// operator sees the failed boot rather than serving traffic with the
	// per-tenant quota and LE breaker disabled.
	if err := EnrollmentRedisRequired(getenv); err != nil {
		return fmt.Errorf("custom-domain wire-up: %w", err)
	}
	cdHandler, cdCleanup := buildCustomDomainHandler(ctx, getenv)
	defer cdCleanup()
	if cdHandler != nil {
		// SIN-62259 routes are mounted at the root of the public mux. The
		// handler returned by buildCustomDomainHandler already includes the
		// /static/ tree.
		mux.Handle("/", cdHandler)
		log.Printf("crm: custom-domain UI mounted on public listener")
	}
	// SIN-62237 / F29 — every public response carries a fresh CSP nonce.
	// Caddy intentionally defers CSP to this Go middleware so the nonce
	// can be per-request (deploy/caddy/security-headers.caddy §"CSP is
	// intentionally NOT here"). The middleware wraps the entire public
	// mux so static assets, HTMX fragments, and full-page renders all
	// inherit the policy.
	//
	// SIN-62331 F51 — the slug RedirectHandler wraps the post-CSP
	// handler so a request whose Host is `<old>.<primary>` is answered
	// with 301 + Clear-Site-Data BEFORE any normal route runs. The
	// handler delegates to the inner mux on miss so /health, the
	// custom-domain UI, and webhook intake stay reachable.
	publicHandler := slugWiring.redirect(csp.Middleware(mux))

	srv := &http.Server{
		Addr:              addr,
		Handler:           publicHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// SIN-62300 reconciler worker — runs alongside the HTTP server with
	// the same context so the shutdown order is: ctx cancel → drain HTTP
	// → wait for the worker to exit. The worker's sweep errors are
	// non-fatal (next tick retries); only an unrecoverable runtime error
	// surfaces here.
	var (
		workerWG  sync.WaitGroup
		workerMu  sync.Mutex
		workerErr error
	)
	if wh != nil && wh.RunWorker != nil {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			if err := wh.RunWorker(ctx); err != nil {
				workerMu.Lock()
				if workerErr == nil {
					workerErr = err
				}
				workerMu.Unlock()
			}
		}()
	}

	log.Printf("crm: public listener on %s", addr)
	srvErr := srv.ListenAndServe()
	workerWG.Wait()
	if srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
		return srvErr
	}
	workerMu.Lock()
	defer workerMu.Unlock()
	if workerErr != nil {
		return fmt.Errorf("webhook reconciler: %w", workerErr)
	}
	return nil
}

// runInternal serves ONLY /internal/tls/ask. Any other path returns 404.
// Caddy reaches this listener via the docker network (compose service
// name "app" + INTERNAL_HTTP_ADDR's port); the host network never sees
// it because compose does not publish the port.
func runInternal(ctx context.Context, addr string, handler http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle(tlsasktransport.Path, handler)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: internal listener on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// dependencies bundles the external clients buildInternalHandler needs.
// It exists so tests can substitute fakes without monkey-patching pgpool
// or goredis package globals.
type dependencies struct {
	pool poolCloser
	rdb  redisCloser
}

type poolCloser interface {
	pgstore.PgxConn
	Close()
}

type redisCloser interface {
	sliding.Cmdable
	Ping(ctx context.Context) *goredis.StatusCmd
	Close() error
}

// dialFn opens the external clients. Production wiring goes through
// pgpool.New + goredis.NewClient; tests inject a stub.
type dialFn func(ctx context.Context, getenv func(string) string) (*dependencies, error)

// defaultDial is the production dialFn.
func defaultDial(ctx context.Context, getenv func(string) string) (*dependencies, error) {
	dsn := getenv(pgpool.EnvDSN)
	redisURL := getenv(envRedisURL)
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	opt, err := goredis.ParseURL(redisURL)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := goredis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &dependencies{pool: pool, rdb: rdb}, nil
}

// buildInternalHandler wires the F45 tls_ask use-case against the
// running process's Postgres + Redis. Returns (nil, no-op) when either
// dep is not configured or unreachable so cmd/server stays bootable in
// environments without those services.
func buildInternalHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	return buildInternalHandlerWith(ctx, getenv, defaultDial)
}

func buildInternalHandlerWith(ctx context.Context, getenv func(string) string, dial dialFn) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	redisURL := getenv(envRedisURL)
	if dsn == "" || redisURL == "" {
		log.Printf("crm: internal listener disabled (DATABASE_URL/REDIS_URL unset)")
		return nil, noop
	}

	deps, err := dial(ctx, getenv)
	if err != nil {
		log.Printf("crm: internal listener disabled — %v", err)
		return nil, noop
	}

	repo := pgstore.NewTLSAskLookup(deps.pool)
	rate := sliding.New(deps.rdb, "customdomain:tls_ask", 3, time.Minute)
	flag := featureflag.NewFromEnv(getenv)
	logger := crmslog.NewTLSAskLogger(slog.Default())
	uc := tls_ask.New(repo, rate, flag, logger, time.Now)
	handler := tlsasktransport.New(uc)

	cleanup := func() {
		deps.pool.Close()
		_ = deps.rdb.Close()
	}
	return handler, cleanup
}
