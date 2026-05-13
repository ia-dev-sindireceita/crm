// Package handler hosts the HTTP request handlers for the CRM httpapi
// adapter. Handlers are pure: every external dependency arrives via
// constructor parameters or an explicit ports interface; nothing reaches
// for a global resolver, db, or filesystem. This keeps each handler
// trivially substitutable in tests and forces wireup decisions to live in
// cmd/server.
package handler

import (
	"encoding/json"
	"net/http"
)

// Health returns the liveness response for the load balancer / k8s probe.
// Mounted OUTSIDE the tenant-scope and auth chains: it must answer 200
// even when the database is down or the host is unrecognised, otherwise
// the LB removes the pod and we lose visibility into the failure.
func Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
