package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestInboxChannelProviderForHealth_ConcurrentStoreLoad pins the SIN-63825
// fix for the race detector failure on PR #273 — a bare package-level
// string was being written by runWith (cmd/server/main.go) and read by
// the healthHandler closure on every /health probe. Sibling tests
// hammering both code paths surfaced a real data race because the
// production happens-before via http.Server's accept-loop does not
// extend across tests that share the package global.
//
// This test runs the Store and Load paths concurrently so any future
// regression to a non-atomic type fails under `go test -race`.
func TestInboxChannelProviderForHealth_ConcurrentStoreLoad(t *testing.T) {
	t.Parallel()
	const iterations = 500
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			v := "llmcustomer"
			inboxChannelProviderForHealth.Store(&v)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			rec := httptest.NewRecorder()
			healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("healthHandler status = %d, want %d", rec.Code, http.StatusOK)
				return
			}
		}
	}()

	wg.Wait()
}
