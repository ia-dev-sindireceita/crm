package authz

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/iam"
)

// Metrics is the small Prometheus surface AuditRecorder maintains. The
// instruments split deliberately into a low-cardinality overview
// (authz_decisions_total) and a deny-only, per-user counter
// (authz_user_deny_total) that backs the probing alert from ADR 0004 §6.
//
// Cardinality reasoning:
//
//   - authz_decisions_total{action, reason_code, outcome} — bounded by
//     the enum sizes declared in iam (Action × ReasonCode × {deny,allow}).
//     This is the dashboard counter; emit on every recorded decision.
//   - authz_user_deny_total{actor_user_id, tenant_id} — labelled by
//     identity. New series only appear when a deny actually happens
//     for a user, so growth is bounded by the count of users who hit
//     at least one deny in the retention window. Probing attempts make
//     this grow proportional to attacker volume, which is exactly what
//     the alert needs to see. Allows do NOT carry user labels, keeping
//     the global cardinality budget away from the happy path.
type Metrics struct {
	Decisions *prometheus.CounterVec
	UserDeny  *prometheus.CounterVec
}

// NewMetrics constructs the two CounterVecs and registers them on reg.
// reg may be nil — then the vectors are returned unregistered, which is
// the pattern tests use when isolation matters more than wiring into
// the global registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authz_decisions_total",
			Help: "Authorizer decisions recorded by the audit wrapper, partitioned by action, reason code, and outcome. Deny=100%; allow is sampled per ADR 0004 §6.",
		}, []string{"action", "reason_code", "outcome"}),
		UserDeny: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authz_user_deny_total",
			Help: "Authorizer deny decisions partitioned by actor and tenant. Backs the AuthzProbingHorizontal alert (sum by (actor_user_id) of 1m rate > 10/min). Grows with attacker activity; not emitted for allows.",
		}, []string{"actor_user_id", "tenant_id"}),
	}
	if reg != nil {
		reg.MustRegister(m.Decisions, m.UserDeny)
	}
	return m
}

// Observe increments the two counters for d. p supplies the actor and
// tenant; the action supplies the low-cardinality dashboard label.
//
// The wrapper calls Observe AFTER the inner Authorizer has returned;
// observability never races the security verdict.
func (m *Metrics) Observe(p iam.Principal, action iam.Action, d iam.Decision) {
	if m == nil {
		return
	}
	outcome := "deny"
	if d.Allow {
		outcome = "allow"
	}
	m.Decisions.WithLabelValues(string(action), string(d.ReasonCode), outcome).Inc()
	if !d.Allow {
		m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String()).Inc()
	}
}
