// Package main is the CRM HTTP server entrypoint (SIN-62208 Fase 0 PR1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

const defaultAddr = ":8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Getenv))
}

// execute picks the run mode based on the environment:
//
//   - DATABASE_URL set → full app mode (assemble deps, mount /login).
//   - DATABASE_URL unset → health-only mode (existing run()) so local
//     liveness probes and the existing TestRun_* suite keep working
//     without a Postgres dependency.
//
// SIN-62348 wires the production app-mode path; the fallback stays
// for backwards compatibility and ops smoke probes.
func execute(ctx context.Context, getenv func(string) string) int {
	addr := defaultAddr
	if v := getenv("HTTP_ADDR"); v != "" {
		addr = v
	}
	logger := slog.Default()
	var err error
	if getenv(postgresadapter.EnvDSN) != "" {
		err = runApp(ctx, addr, getenv, logger)
	} else {
		err = run(ctx, addr)
	}
	if err != nil {
		log.Printf("crm: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: listening on %s", addr)
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
