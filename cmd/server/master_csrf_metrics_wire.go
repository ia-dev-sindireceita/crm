package main

// SIN-65277 — observability follow-up to SIN-65264 / PR #372 (SecEng + CTO
// review residual). The RequireMasterOriginCSRF gate (master_csrf.go) exposes
// an OnReject(*http.Request, OriginCSRFReason) hook whose reasons are
// documented as stable Prometheus labels, but cmd/server left it nil — CSRF
// rejections were only logged (master_csrf_rejected slog event), never
// counted. A counter on rejection reason lets dashboards/alerts spot a
// CSRF-probe campaign against the master operator surface (which fronts
// impersonate) without grepping logs.
//
// No security behaviour changes here: the gate is already fail-closed and
// fully functional. This wires a metrics-only side effect onto its existing
// reject path.

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// masterCSRFRejectReasons enumerates the stable OriginCSRFReason label values
// so the counter pre-initialises one zero-valued series per reason at boot.
// Pre-seeding makes rate()/increase() well-defined from the first scrape and
// keeps a dashboard panel from rendering "No data" until the first rejection.
var masterCSRFRejectReasons = []mastermfa.OriginCSRFReason{
	mastermfa.OriginCSRFReasonHostUnset,
	mastermfa.OriginCSRFReasonMissing,
	mastermfa.OriginCSRFReasonUnparsable,
	mastermfa.OriginCSRFReasonSchemeNotHTTPS,
	mastermfa.OriginCSRFReasonMismatch,
}

// newMasterCSRFRejectMetric builds the master_csrf_rejected_total CounterVec,
// registers it on reg, and returns it alongside an OnReject closure suitable
// for mastermfa.RequireMasterOriginCSRFConfig.OnReject. The middleware stays
// decoupled from Prometheus — the composition root owns the instrument and
// hands the gate only the func(*http.Request, OriginCSRFReason) hook.
//
// reg may be nil (the unregistered pattern tests use); production passes
// prometheus.DefaultRegisterer so the counter rides the existing /metrics
// scrape endpoint. The CounterVec is returned so wire tests can assert the
// exact series the live gate increments.
//
// Cardinality: the only label is reason, bounded by the five OriginCSRFReason
// constants — a fixed, small series set with no identity or path labels, so
// an attacker cannot inflate cardinality by varying the request.
func newMasterCSRFRejectMetric(reg prometheus.Registerer) (*prometheus.CounterVec, func(*http.Request, mastermfa.OriginCSRFReason)) {
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "master_csrf_rejected_total",
		Help: "Master operator surface Origin/Referer CSRF rejections, partitioned by reason. Backs detection of a CSRF-probe campaign against the master console (which fronts impersonate). Fail-closed gate; non-zero means a forged or malformed cross-origin POST was blocked.",
	}, []string{"reason"})
	if reg != nil {
		reg.MustRegister(vec)
	}
	for _, reason := range masterCSRFRejectReasons {
		vec.WithLabelValues(string(reason))
	}
	onReject := func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		vec.WithLabelValues(string(reason)).Inc()
	}
	return vec, onReject
}
