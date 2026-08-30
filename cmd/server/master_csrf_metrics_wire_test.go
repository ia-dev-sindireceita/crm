package main

// SIN-65277 — wire test pinning the master_csrf_rejected_total counter to the
// real RequireMasterOriginCSRF gate. A forged-origin POST (Origin = a tenant
// host ≠ the canonical master host) must be rejected with 403 AND increment
// the counter on the exact reason label, proving cmd/server's OnReject hook is
// threaded through the live middleware rather than left nil as before.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

func TestNewMasterCSRFRejectMetric_ForgedOriginPOSTIncrementsCounter(t *testing.T) {
	t.Parallel()

	// Unregistered registry seam (reg=nil) keeps this test isolated from the
	// global registry the production path uses.
	vec, onReject := newMasterCSRFRejectMetric(nil)

	// Wire the REAL gate with the production OnReject closure.
	const masterHost = "master.crm.local"
	gate := mastermfa.RequireMasterOriginCSRF(mastermfa.RequireMasterOriginCSRFConfig{
		MasterHost: masterHost,
		Logger:     quietLogger(),
		OnReject:   onReject,
	})
	reached := false
	h := gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// Forged same-site POST: a tenant subdomain page submitting to the master
	// surface. Host shares the registrable domain, so SameSite does not isolate
	// it; Origin verification is what blocks it → reason "mismatch".
	req := httptest.NewRequest(http.MethodPost, "/m/login", nil)
	req.Header.Set("Origin", "https://tenant.crm.local")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged-origin POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if reached {
		t.Fatal("inner handler ran on a forged-origin POST — gate failed open")
	}

	got := testutil.ToFloat64(vec.WithLabelValues(string(mastermfa.OriginCSRFReasonMismatch)))
	if got != 1 {
		t.Fatalf("master_csrf_rejected_total{reason=%q} = %v, want 1 — "+
			"cmd/server must thread OnReject into the live RequireMasterOriginCSRF gate",
			mastermfa.OriginCSRFReasonMismatch, got)
	}
}

func TestNewMasterCSRFRejectMetric_RegistersAndPreSeedsReasons(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	vec, _ := newMasterCSRFRejectMetric(reg)
	if vec == nil {
		t.Fatal("newMasterCSRFRejectMetric returned a nil CounterVec")
	}

	// Every stable reason is pre-seeded at 0 so rate()/increase() are defined
	// from the first scrape and no panel renders "No data".
	for _, reason := range masterCSRFRejectReasons {
		if got := testutil.ToFloat64(vec.WithLabelValues(string(reason))); got != 0 {
			t.Errorf("reason %q pre-seeded value = %v, want 0", reason, got)
		}
	}

	// The vector is registered on reg (so /metrics scrapes it). Gathering must
	// surface the metric family.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("reg.Gather: %v", err)
	}
	found := false
	for _, fam := range families {
		if fam.GetName() == "master_csrf_rejected_total" {
			found = true
			if len(fam.GetMetric()) != len(masterCSRFRejectReasons) {
				t.Errorf("master_csrf_rejected_total series = %d, want %d (one per reason)",
					len(fam.GetMetric()), len(masterCSRFRejectReasons))
			}
		}
	}
	if !found {
		t.Fatal("master_csrf_rejected_total not registered on the provided registry")
	}
}

func TestNewMasterCSRFRejectMetric_NilRegistererDoesNotPanic(t *testing.T) {
	t.Parallel()
	if vec, onReject := newMasterCSRFRejectMetric(nil); vec == nil || onReject == nil {
		t.Fatal("nil registerer must still return a usable vec + closure")
	}
}
