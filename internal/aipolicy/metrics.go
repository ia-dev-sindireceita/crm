package aipolicy

import "github.com/prometheus/client_golang/prometheus"

// AuditMetrics is the per-event observability surface for the audit
// pipeline (SIN-62353 / decisão #8). One CounterVec, labelled by the
// scope and field that changed, so dashboards can answer "how often
// does AIEnabled flip per tenant" without scraping the audit table.
//
// Cardinality: scope_type ∈ {tenant, team, channel} (3) × field set
// (8 = 7 ai_policy columns + __created__ + __deleted__) × actor_master
// {true, false} (2) → 48 series per tenant on the worst case. The
// dashboard aggregates across tenants so the active series count
// stays bounded by the field set; tenant_id is deliberately NOT a
// label (the audit_log table is the long-term store for per-tenant
// rollups).
type AuditMetrics struct {
	Emitted *prometheus.CounterVec
}

// NewAuditMetrics builds the metric and registers it on reg. reg may
// be nil — then the counter is returned unregistered, which is the
// pattern tests use to keep registries isolated.
func NewAuditMetrics(reg prometheus.Registerer) *AuditMetrics {
	m := &AuditMetrics{
		Emitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_policy_audit_emitted_total",
			Help: "Per-field ai_policy_audit rows emitted by the RecordingRepository decorator, partitioned by scope, field, and whether the change happened inside a master impersonation session.",
		}, []string{"scope_type", "field", "actor_master"}),
	}
	if reg != nil {
		reg.MustRegister(m.Emitted)
	}
	return m
}

// Observe increments the Emitted counter for the given event. The
// decorator calls this after the audit row commits so a failed insert
// does not pollute the dashboard.
func (m *AuditMetrics) Observe(ev AuditEvent) {
	if m == nil {
		return
	}
	master := "false"
	if ev.Actor.Master {
		master = "true"
	}
	m.Emitted.WithLabelValues(string(ev.ScopeType), ev.Field, master).Inc()
}
