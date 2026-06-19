package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// SIN-65276 — the OnReject hook newMasterCSRFRejectionCounter returns must
// register master_csrf_rejected_total on the supplied registry and increment
// the series for the rejection cause it is called with.
func TestNewMasterCSRFRejectionCounter_IncrementsByReason(t *testing.T) {
	reg := prometheus.NewRegistry()
	hook := newMasterCSRFRejectionCounter(reg)

	// Two mismatches and one missing — distinct stable label values.
	hook(nil, mastermfa.OriginCSRFReasonMismatch)
	hook(nil, mastermfa.OriginCSRFReasonMismatch)
	hook(nil, mastermfa.OriginCSRFReasonMissing)

	const metric = "master_csrf_rejected_total"
	if got := testutil.CollectAndCount(reg, metric); got != 2 {
		t.Fatalf("series count = %d, want 2 (one per distinct reason)", got)
	}

	expected := `
# HELP master_csrf_rejected_total Master console Origin/Referer CSRF rejections by cause. Backs a CSRF-probe alert on the master operator surface; bounded cardinality (one series per OriginCSRFReason).
# TYPE master_csrf_rejected_total counter
master_csrf_rejected_total{reason="master_csrf.mismatch"} 2
master_csrf_rejected_total{reason="master_csrf.missing"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), metric); err != nil {
		t.Fatalf("metric mismatch: %v", err)
	}
}

// TestNewMasterCSRFRejectionCounter_NilRegistry proves the nil-registry seam
// (used by callers that want an isolated, unregistered counter) does not
// panic and the returned hook is still safe to invoke.
func TestNewMasterCSRFRejectionCounter_NilRegistry(t *testing.T) {
	hook := newMasterCSRFRejectionCounter(nil)
	// Must not panic on a nil registry or a nil request.
	hook(nil, mastermfa.OriginCSRFReasonHostUnset)
}
