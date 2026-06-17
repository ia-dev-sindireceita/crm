package main

// SIN-64985 — the production /health (served by healthHandler via
// newMux, NOT the chi router) must report the web-surface mounted/not
// map published by the router wireup into surfacesForHealth. These tests
// pin the read path: a published map renders, the default (nil) omits.
//
// These tests are intentionally NOT parallel: they Store into the shared
// surfacesForHealth global. Go pauses t.Parallel() tests until the
// sequential tests finish, so storing/restoring here cannot contaminate
// the parallel /health tests that decode the body into map[string]string
// (a surfaces object would break that decode).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler_DefaultOmitsSurfaces(t *testing.T) {
	surfacesForHealth.Store(nil)
	t.Cleanup(func() { surfacesForHealth.Store(nil) })

	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["surfaces"]; ok {
		t.Fatalf("surfaces must be omitted when nothing is published; body=%v", body)
	}
}

func TestHealthHandler_ReportsPublishedSurfaces(t *testing.T) {
	published := map[string]bool{"ai_policy": false, "inbox": true, "contacts": true}
	surfacesForHealth.Store(&published)
	t.Cleanup(func() { surfacesForHealth.Store(nil) })

	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body struct {
		Status   string          `json:"status"`
		Surfaces map[string]bool `json:"surfaces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status=%q, want ok", body.Status)
	}
	for k, want := range published {
		if got, ok := body.Surfaces[k]; !ok || got != want {
			t.Fatalf("surfaces[%q]=%v (present=%v), want %v", k, got, ok, want)
		}
	}
}
