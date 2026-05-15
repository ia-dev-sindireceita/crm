package whatsapp

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/inbox"
)

// envelStatus is the permissive subset of one Meta statuses[] entry.
// Fields we do not consume (recipient_id, conversation, pricing,
// biz_opaque_callback_data) are intentionally absent so an upstream
// schema change in those areas cannot break parsing — encoding/json
// ignores unknown fields by default.
type envelStatus struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Timestamp string            `json:"timestamp"`
	Errors    []envelMessageErr `json:"errors"`
}

// statusMetrics is the small Prometheus surface the status reconciler
// exposes. The instruments live next to the adapter (not in obs/) so
// the package stays self-contained and unit tests can swap in a fresh
// registry without touching shared state.
type statusMetrics struct {
	total *prometheus.CounterVec
	lag   *prometheus.HistogramVec
}

// newStatusMetrics registers two instruments on reg:
//
//   - whatsapp_status_total{status,outcome} — counts every status
//     update the adapter dispatches, labelled by carrier status and
//     domain outcome (applied / noop / unknown_message / dropped).
//   - whatsapp_status_lag_seconds{status} — measures Meta-timestamp →
//     receiver-observed-at lag so dashboards can alert on stale ACKs.
//
// reg MUST be non-nil; production wires prometheus.DefaultRegisterer
// and tests inject a fresh prometheus.NewRegistry() to avoid
// duplicate-registration panics.
func newStatusMetrics(reg prometheus.Registerer) *statusMetrics {
	m := &statusMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_status_total",
			Help: "WhatsApp status updates received, partitioned by carrier status and adapter outcome.",
		}, []string{"status", "outcome"}),
		lag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "whatsapp_status_lag_seconds",
			Help: "Lag between Meta's status timestamp and our reception time, in seconds.",
			// Default web-handler buckets cover the realistic 5ms..10s span
			// of webhook delivery — anything beyond ~10s is a Meta queueing
			// incident and shows up on the >10s bucket.
			Buckets: prometheus.DefBuckets,
		}, []string{"status"}),
	}
	reg.MustRegister(m.total, m.lag)
	return m
}

// observe records one status-update outcome and (when occurredAt is
// known) the receiver lag. Empty occurredAt skips the histogram so
// we do not pollute the metric with zero-lag artefacts.
func (m *statusMetrics) observe(status, outcome string, occurredAt, now time.Time) {
	if m == nil {
		return
	}
	m.total.WithLabelValues(status, outcome).Inc()
	if occurredAt.IsZero() || now.IsZero() {
		return
	}
	lag := now.Sub(occurredAt).Seconds()
	if lag < 0 {
		// Clock skew from Meta's side; record 0 so the histogram still
		// carries a sample but the bucketing does not see a negative.
		lag = 0
	}
	m.lag.WithLabelValues(status).Observe(lag)
}

// deliverStatus processes one envelope status entry. The tenant and
// rate-limit checks have already been applied by deliverChange; this
// method assumes a valid tenant + an enabled feature flag.
//
// All carrier-side dropping (no updater wired, unknown status string,
// malformed wamid) flows through observe() with outcome="dropped" so
// the operator can see how many status updates fall through the
// cracks without scraping log lines. Productive outcomes
// (applied / noop / unknown_message) bump agg.statusesHandled so the
// handler-level latency histogram can label a status-only envelope
// as "status_processed" instead of "dropped_empty".
func (a *Adapter) deliverStatus(ctx context.Context, tenantID uuid.UUID, pnID string, st envelStatus, agg *handlerAgg) {
	now := a.clock.Now()

	if a.statusUpdater == nil {
		a.logger.Warn("whatsapp.status_updater_unwired",
			slog.String("tenant_id", tenantID.String()),
			slog.String("status", st.Status))
		a.statusMetrics.observe(normaliseStatus(st.Status), "dropped", time.Time{}, now)
		agg.otherDrops++
		return
	}
	wamid := strings.TrimSpace(st.ID)
	if wamid == "" {
		a.logger.Warn("whatsapp.status_missing_wamid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID))
		a.statusMetrics.observe(normaliseStatus(st.Status), "dropped", time.Time{}, now)
		agg.otherDrops++
		return
	}
	status, ok := mapWhatsAppStatus(st.Status)
	if !ok {
		a.logger.Warn("whatsapp.status_unknown_value",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid),
			slog.String("status", st.Status))
		a.statusMetrics.observe(normaliseStatus(st.Status), "dropped", time.Time{}, now)
		agg.otherDrops++
		return
	}

	occurredAt := parseMetaTimestamp(st.Timestamp)
	ev := inbox.StatusUpdate{
		TenantID:          tenantID,
		Channel:           Channel,
		ChannelExternalID: wamid,
		NewStatus:         status,
		OccurredAt:        occurredAt,
	}
	if status == inbox.MessageStatusFailed && len(st.Errors) > 0 {
		ev.ErrorCode = st.Errors[0].Code
		ev.ErrorTitle = st.Errors[0].Title
	}

	callCtx, cancel := context.WithTimeout(ctx, a.cfg.DeliverTimeout)
	defer cancel()

	res, err := a.statusUpdater.HandleStatus(callCtx, ev)
	if err != nil {
		a.logger.Error("whatsapp.status_deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID),
			slog.String("wamid", wamid),
			slog.String("status", string(status)),
			slog.String("err", err.Error()))
		a.statusMetrics.observe(string(status), "error", occurredAt, now)
		agg.deliverErrors++
		return
	}

	outcome := string(res.Outcome)
	switch res.Outcome {
	case inbox.StatusOutcomeApplied:
		a.logger.Info("whatsapp.status_applied",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid),
			slog.String("from", string(res.PreviousStatus)),
			slog.String("to", string(res.NewStatus)))
	case inbox.StatusOutcomeNoop:
		a.logger.Debug("whatsapp.status_noop",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid),
			slog.String("status", string(res.NewStatus)))
	case inbox.StatusOutcomeUnknownMessage:
		a.logger.Info("whatsapp.status_unknown_message",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid),
			slog.String("status", string(res.NewStatus)))
	default:
		outcome = "unknown"
	}

	if status == inbox.MessageStatusFailed && ev.ErrorCode != 0 {
		a.logger.Warn("whatsapp.status_failed_error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid),
			slog.Int("error_code", ev.ErrorCode),
			slog.String("error_title", ev.ErrorTitle))
	}

	a.statusMetrics.observe(string(status), outcome, occurredAt, now)
	agg.statusesHandled++
}

// mapWhatsAppStatus normalises Meta's lower-case status string onto
// the domain enum. Unknown values return false so the caller can
// surface them as "dropped" — the carrier occasionally introduces new
// values (e.g. "deleted") that we want to log without crashing.
func mapWhatsAppStatus(raw string) (inbox.MessageStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sent":
		return inbox.MessageStatusSent, true
	case "delivered":
		return inbox.MessageStatusDelivered, true
	case "read":
		return inbox.MessageStatusRead, true
	case "failed":
		return inbox.MessageStatusFailed, true
	default:
		return "", false
	}
}

// normaliseStatus is the label-side normaliser for the metrics. We
// drop unknown values onto "unknown" so the label cardinality is
// bounded.
func normaliseStatus(raw string) string {
	if s, ok := mapWhatsAppStatus(raw); ok {
		return string(s)
	}
	return "unknown"
}
