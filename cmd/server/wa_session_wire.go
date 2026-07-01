package main

// SIN-66258 — WhatsApp session (non-official, whatsmeow) Fase 3 wireup.
//
// This composition root mounts the Fase 1 session Manager
// (internal/wasession) and the Fase 2 inbox adapter
// (internal/adapter/channels/wa_session) into cmd/server, mirroring the
// shape of whatsapp_wire.go. The two WhatsApp transports COEXIST: the
// official Meta Cloud channel (whatsapp_wire.go) keeps serving the
// majority of tenants while the non-official session serves opted-in
// tenants ("clientes específicos") — plan rev 4 of SIN-66252, ratified
// by ADR 0107, board-accepted ToS/ban risk on 2026-06-29.
//
// Reversibility / blast radius (the task's lens):
//
//   - Deny-by-default. buildWASessionWiring returns nil unless
//     FEATURE_WA_SESSION_ENABLED=1. Flag off (the default) => no
//     Manager, no goroutines, no DB connection, no behaviour change.
//   - Opt-in per tenant. Sessions are started ONLY for the explicit
//     FEATURE_WA_SESSION_TENANTS allowlist, and both inbound delivery
//     and outbound send re-check the per-tenant wa_session feature flag,
//     so a non-listed tenant is inert on both directions.
//   - The official channel is never touched. Inbound from the session
//     lands on the SAME inbox (Channel == "whatsapp", ADR 0107 D4) so a
//     contact's thread is unified; outbound provider selection is an
//     additive decorator (providerRoutingOutbound) the send path can opt
//     into without disabling Meta.
//   - Separate credential store. The whatsmeow session credentials live
//     in their own Postgres pointed at by WA_SESSION_DATABASE_URL; this
//     wire never creates whatsmeow_* tables in the app database.
//
// No PII in logs: this wire logs tenant ids and session status only —
// never phone numbers, message bodies, or QR pairing codes (QR.Code is a
// Credential and is deliberately not surfaced here).

import (
	"context"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	wasessionchan "github.com/pericles-luz/crm/internal/adapter/channels/wa_session"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/wasession"
	"github.com/pericles-luz/crm/internal/wasession/whatsmeowdev"
)

// envWASessionDSN points at the dedicated Postgres holding the whatsmeow
// credential store (ADR 0107 D3). It is intentionally separate from
// DATABASE_URL so the session library's own tables never land in the app
// schema; a missing value disables the transport (deny-by-default).
const envWASessionDSN = "WA_SESSION_DATABASE_URL"

// waSessionShutdownTimeout bounds how long Cleanup waits for the
// supervisor goroutines to drain before the process exits anyway.
const waSessionShutdownTimeout = 5 * time.Second

// waSessionWiring bundles the artifacts buildWASessionWiring produces.
// Start launches the inbound pump and the per-tenant sessions; Cleanup
// stops the pump, shuts the Manager down, and releases the pool. Session
// is the outbound adapter for the session transport, and RouteOutbound
// wraps a primary (official) channel with per-tenant provider selection.
type waSessionWiring struct {
	Start   func()
	Cleanup func()
	Session inbox.OutboundChannel

	// Provisioner is the Fase 4 (SIN-66259) provisioning seam: it adapts
	// the Manager + QR cache into the web/wasession.Provisioner port so the
	// HTMX UI can show status / QR and connect / disconnect a tenant
	// session without ever touching whatsmeow. Nil-safe to read; only set
	// when the transport is mounted.
	Provisioner *managerProvisioner

	flag   wasessionchan.FeatureFlag
	logger *slog.Logger
}

// RouteOutbound returns an OutboundChannel that sends through the session
// transport for tenants whose wa_session flag is on and delegates to
// primary (the official Meta Cloud channel) for everyone else. The send
// path opts into coexistence by wrapping its primary channel with this;
// when the session is disabled the flag denies every tenant so the
// wrapper is a transparent pass-through to primary.
func (w *waSessionWiring) RouteOutbound(primary inbox.OutboundChannel) inbox.OutboundChannel {
	return providerRoutingOutbound{
		primary: primary,
		session: w.Session,
		flag:    w.flag,
		logger:  w.logger,
	}
}

// buildWASessionWiring assembles the production session transport.
// Returns nil — "skip mounting the session" — when the global flag is
// off or any required dependency is missing, matching the fail-soft
// pattern of the other cmd/server wires.
// metricsOpt is an optional trailing dependency (same backward-compatible
// variadic pattern the inbound pump uses for its QR sink): production passes
// the boot-time *obs.Metrics so status transitions increment
// wa_session_status_transitions_total, while the pre-Fase-5 unit call sites
// that omit it keep compiling and run with no metric (a nil observer is an
// inert pass-through tee). Only the first value is honoured.
func buildWASessionWiring(ctx context.Context, getenv func(string) string, metricsOpt ...*obs.Metrics) *waSessionWiring {
	logger := slog.Default()
	var statusObs waStatusObserver
	if len(metricsOpt) > 0 && metricsOpt[0] != nil {
		statusObs = metricsOpt[0]
	}
	if !waSessionGloballyEnabled(getenv) {
		log.Printf("crm: wa session disabled (FEATURE_WA_SESSION_ENABLED != 1)")
		return nil
	}
	appDSN := getenv(pgpool.EnvDSN)
	if appDSN == "" {
		log.Printf("crm: wa session disabled (DATABASE_URL unset)")
		return nil
	}
	waDSN := strings.TrimSpace(getenv(envWASessionDSN))
	if waDSN == "" {
		log.Printf("crm: wa session disabled (WA_SESSION_DATABASE_URL unset)")
		return nil
	}
	// Fail-closed (SIN-66276): globally enabled but no tenant allowlist is
	// an operator misconfiguration, not a fleet-wide enable. Refuse to mount
	// the transport — this is the composition-root half of the
	// defense-in-depth pair with EnvFeatureFlag.Enabled, so a stray empty
	// FEATURE_WA_SESSION_TENANTS can never route outbound WhatsApp for the
	// whole fleet onto the unofficial session. Checked before opening any
	// pool/whatsmeow store so the misconfig fails fast and cheap.
	if len(parseWASessionTenants(getenv)) == 0 {
		log.Printf("crm: wa session disabled — FEATURE_WA_SESSION_ENABLED=1 but FEATURE_WA_SESSION_TENANTS empty (fail-closed; name the tenants to enable)")
		return nil
	}
	pool, err := pgpool.New(ctx, appDSN)
	if err != nil {
		log.Printf("crm: wa session disabled — pg connect: %v", err)
		return nil
	}
	receiver, err := buildWASessionReceiver(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session disabled — receiver: %v", err)
		return nil
	}
	factory, err := whatsmeowdev.Open(ctx, waDSN)
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session disabled — whatsmeow store: %v", err)
		return nil
	}
	manager := wasession.NewManager(factory, wasession.WithLogger(logger))
	qrCache := wasession.NewQRCache()
	flag := wasessionchan.NewEnvFeatureFlag(getenv)
	adapter, err := assembleWASessionAdapter(
		receiver,
		managerSessionSender{dispatch: manager},
		flag,
		wasessionchan.NewInMemoryRateLimiter(),
		wasessionchan.ConfigFromEnv(getenv),
		logger,
	)
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session disabled — assemble: %v", err)
		return nil
	}

	// SIN-66305 (R3): tamper-evident audit of terminal session transitions.
	// Built over the app_runtime pool (the auditor pins app.tenant_id via
	// WithTenant so the background write satisfies RLS) and attributed to the
	// reserved system principal. Fail-soft: a construction error disables the
	// audit tee but still mounts the transport, matching the wire's pattern.
	var transitionAuditor waTransitionAuditor
	if a, err := pgpool.NewWASessionTransitionAuditor(pool, iam.SystemPrincipalID()); err != nil {
		log.Printf("crm: wa session transition audit disabled — auditor: %v", err)
	} else {
		transitionAuditor = a
	}

	tenants := parseWASessionTenants(getenv)
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	start := func() {
		// SIN-66260 Fase 5: tee the Manager's event stream through the
		// status-transition metric before the inbound pump consumes it, so
		// to="banned"/"disconnected" become production-observable
		// (WASessionBanned alert). observeWAStatus is a transparent
		// forwarder when statusObs is nil, so the pump's contract is
		// unchanged and the flag-off boot stays inert.
		//
		// SIN-66305 (R3): a second tee (auditWAStatus) records the
		// tamper-evident audit row on ban/disconnect, then forwards every
		// event to the pump. Chained after the metric tee so both observe
		// the same stream; nil auditor is a transparent pass-through.
		events := auditWAStatus(pumpCtx, observeWAStatus(pumpCtx, manager.Events(), statusObs), transitionAuditor, logger)
		go pumpWASessionInbound(pumpCtx, events, adapter, logger, qrCache)
		for _, t := range tenants {
			if err := manager.StartSession(context.Background(), t); err != nil {
				logger.Warn("wa_session.start_failed",
					slog.String("tenant_id", t.String()))
			}
		}
		// No empty-allowlist branch here: buildWASessionWiring refuses to
		// mount when FEATURE_WA_SESSION_TENANTS is empty (fail-closed,
		// SIN-66276), so tenants is always non-empty by this point.
	}
	cleanup := func() {
		pumpCancel()
		shutCtx, c := context.WithTimeout(context.Background(), waSessionShutdownTimeout)
		defer c()
		_ = manager.Shutdown(shutCtx)
		pool.Close()
	}
	log.Printf("crm: wa session transport mounted (tenants=%d)", len(tenants))
	return &waSessionWiring{
		Start:       start,
		Cleanup:     cleanup,
		Session:     adapter,
		Provisioner: &managerProvisioner{ctrl: manager, qr: qrCache},
		flag:        flag,
		logger:      logger,
	}
}

// buildWASessionReceiver assembles the receive-inbound use case from a
// connected pool — the same storage path the official WhatsApp wire
// uses, so the session and Meta inbound flows share dedup, contact
// upsert, and conversation persistence (ADR 0107 D4).
func buildWASessionReceiver(pool *pgxpool.Pool) (*inboxusecase.ReceiveInbound, error) {
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, err
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, err
	}
	contactsUC, err := contactsusecase.New(contactsStore)
	if err != nil {
		return nil, err
	}
	receiver, err := inboxusecase.NewReceiveInbound(inboxStore, inboxStore, contactsUC)
	if err != nil {
		return nil, err
	}
	// SIN-66378 P4 — route new conversations to the tenant channel
	// instance resolved from the inbound identity (channel_id). Soft-fail.
	wireChannelResolver(receiver, pool)
	return receiver, nil
}

// assembleWASessionAdapter constructs the inbox adapter from already-built
// dependencies. Split out so unit tests wire fakes instead of a live
// whatsmeow Manager / pgx pool.
func assembleWASessionAdapter(
	receiver inbox.InboundChannel,
	sender wasessionchan.SessionSender,
	flag wasessionchan.FeatureFlag,
	rate wasessionchan.RateLimiter,
	cfg wasessionchan.Config,
	logger *slog.Logger,
) (*wasessionchan.Adapter, error) {
	return wasessionchan.New(receiver, sender, flag, rate,
		wasessionchan.WithLogger(logger),
		wasessionchan.WithConfig(cfg),
	)
}

// sessionInboundReceiver is the narrow port the inbound pump drives. The
// Fase 2 *wa_session.Adapter satisfies it; tests inject a recording fake.
type sessionInboundReceiver interface {
	Receive(ctx context.Context, msg wasessionchan.SessionMessage) error
}

// waStatusObserver is the narrow metric seam observeWAStatus drives: one
// call per session status transition (ADR 0107 / SIN-66260 Fase 5). The
// boot *obs.Metrics satisfies it via WASessionStatusTransition; tests inject
// a counting fake. Kept tiny so cmd/server stays unaware of the prometheus
// surface beyond this one method.
type waStatusObserver interface {
	WASessionStatusTransition(to string)
}

// observeWAStatus tees the Manager's event stream: it increments the
// status-transition metric on every EventStatus, then forwards EVERY event
// (inbound, status, QR) downstream to the inbound pump in order. This keeps
// the ban-observability concern in the composition root — the pump and its
// existing tests stay untouched — while guaranteeing that a terminal
// to="banned" transition is recorded for the WASessionBanned alert.
//
// A nil obs makes it a transparent pass-through (the source channel is
// returned unwrapped), so flag-off / metric-less boots add no goroutine and
// no behaviour change. Manager.Events() is never closed, so the relay exits
// only when ctx (the pump context) is cancelled at shutdown; the returned
// channel is closed on exit so the pump's range/select terminates cleanly.
func observeWAStatus(ctx context.Context, src <-chan wasession.Event, obs waStatusObserver) <-chan wasession.Event {
	if obs == nil {
		return src
	}
	out := make(chan wasession.Event, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				if ev.Kind == wasession.EventStatus && ev.Status != nil {
					obs.WASessionStatusTransition(ev.Status.To.String())
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// waTransitionAuditor is the narrow audit seam auditWAStatus drives: one
// tamper-evident audit_log_security row per TERMINAL session transition
// (SIN-66305 / R3). The postgres *WASessionTransitionAuditor satisfies it;
// tests inject a recording fake. Kept tiny so cmd/server stays unaware of
// the audit storage surface beyond this one method.
type waTransitionAuditor interface {
	RecordTransition(ctx context.Context, tenantID uuid.UUID, evt audit.SecurityEvent, from, to, reason string) error
}

// auditWAStatus tees the event stream a second time (after observeWAStatus):
// on a transition to "banned" or "disconnected" it writes ONE audit row,
// then forwards EVERY event downstream to the inbound pump in order. The
// audit concern lives here in the composition root so the pump and its
// existing tests stay untouched (the pump signature is unchanged).
//
// Only ban/disconnect are audited — the LGPD-grade non-repudiation case the
// security review (SIN-66260 R3) asked for. connected/pairing/unpaired carry
// no comparable repudiation weight and stay metric-only (#432). A nil auditor
// makes this a transparent pass-through (the source channel is returned
// unwrapped), so flag-off / audit-less boots add no goroutine.
//
// A failed audit write is logged and the event is STILL forwarded — losing
// the inbound stream because the audit DB hiccuped would be a worse failure
// than a gap in the trail (which the structured log + metric still cover).
// The write is synchronous (non-repudiation) but bounded by ctx, which is
// the pump context cancelled at shutdown.
func auditWAStatus(ctx context.Context, src <-chan wasession.Event, auditor waTransitionAuditor, logger *slog.Logger) <-chan wasession.Event {
	if auditor == nil {
		return src
	}
	if logger == nil {
		logger = slog.Default()
	}
	out := make(chan wasession.Event, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				if ev.Kind == wasession.EventStatus && ev.Status != nil {
					if evt, ok := waTerminalAuditEvent(ev.Status.To); ok {
						if err := auditor.RecordTransition(ctx, ev.TenantID, evt,
							ev.Status.From.String(), ev.Status.To.String(), ev.Status.Reason); err != nil {
							logger.Warn("wa_session.transition_audit_failed",
								slog.String("tenant_id", ev.TenantID.String()),
								slog.String("to", ev.Status.To.String()))
						}
					}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// waTerminalAuditEvent maps the terminal session states to their audit
// SecurityEvent. Non-terminal transitions return ok=false (not audited).
func waTerminalAuditEvent(to wasession.Status) (audit.SecurityEvent, bool) {
	switch to {
	case wasession.StatusBanned:
		return audit.SecurityEventWASessionBanned, true
	case wasession.StatusDisconnected:
		return audit.SecurityEventWASessionDisconnected, true
	default:
		return "", false
	}
}

// pumpWASessionInbound drains the Manager's fan-out channel and translates
// each EventInbound into the carrier-neutral SessionMessage the Fase 2
// adapter expects, then hands it to Receive (which applies the border
// drops, normalisation and domain dedup). Status changes are logged
// (tenant + state only); QR events are acknowledged WITHOUT logging the
// secret pairing code. It returns when ctx is cancelled or the channel
// is closed.
// qr is a trailing variadic so the pre-Fase-4 4-arg call sites keep
// compiling (the same backward-compatible pattern dashboard_wire uses for
// its optional userLabels); production passes the QRCache as the single
// value. Only the first sink is honoured; nil disables QR caching.
func pumpWASessionInbound(ctx context.Context, events <-chan wasession.Event, rcv sessionInboundReceiver, logger *slog.Logger, qr ...qrSink) {
	if logger == nil {
		logger = slog.Default()
	}
	var sink qrSink
	if len(qr) > 0 {
		sink = qr[0]
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case wasession.EventInbound:
				if ev.Inbound == nil {
					continue
				}
				msg := wasessionchan.SessionMessage{
					TenantID:    ev.TenantID,
					MessageID:   ev.Inbound.ExternalID,
					SenderPhone: ev.Inbound.SenderE164,
					SenderName:  ev.Inbound.SenderName,
					Body:        ev.Inbound.Body,
					Timestamp:   ev.Inbound.OccurredAt,
					HasMedia:    ev.Inbound.HasMedia,
					FromMe:      ev.Inbound.FromMe,
					// IsGroup is not surfaced by the Fase 1 event;
					// whatsmeowdev filters group JIDs upstream.
				}
				if err := rcv.Receive(ctx, msg); err != nil {
					logger.Warn("wa_session.inbound_deliver_failed",
						slog.String("tenant_id", ev.TenantID.String()))
				}
			case wasession.EventStatus:
				if ev.Status != nil {
					// Once the session pairs (connected) or is logged out
					// (banned), the pending QR is dead — drop it so the
					// provisioning UI stops offering a stale code.
					if sink != nil {
						switch ev.Status.To {
						case wasession.StatusConnected, wasession.StatusBanned:
							sink.Clear(ev.TenantID)
						}
					}
					logger.Info("wa_session.status",
						slog.String("tenant_id", ev.TenantID.String()),
						slog.String("to", ev.Status.To.String()))
				}
			case wasession.EventQR:
				// QR.Code is a Credential — never logged (ADR 0107 D6). It
				// is cached (not logged) so the Fase 4 provisioning UI can
				// render it on demand; the cache entry expires at the QR's
				// rotation deadline.
				if sink != nil && ev.QR != nil {
					sink.Put(ev.TenantID, *ev.QR)
				}
				logger.Info("wa_session.qr_pending",
					slog.String("tenant_id", ev.TenantID.String()))
			}
		}
	}
}

// managerSessionSender bridges the Fase 1 Manager to the Fase 2
// SessionSender port. The adapter validates and hands a '+'-prefixed
// E.164 string; the Manager's Send (and the whatsmeow device beneath it)
// expects bare E.164 with no '+', so the bridge strips it.
type managerSessionSender struct {
	dispatch sessionDispatcher
}

// sessionDispatcher is the Manager.Send seam, narrowed so tests can drive
// the bridge without a live Manager.
type sessionDispatcher interface {
	Send(ctx context.Context, tenantID uuid.UUID, toE164, body string) (string, error)
}

// SendText implements wa_session.SessionSender.
func (s managerSessionSender) SendText(ctx context.Context, tenantID uuid.UUID, toE164, body string) (string, error) {
	return s.dispatch.Send(ctx, tenantID, strings.TrimPrefix(toE164, "+"), body)
}

// providerRoutingOutbound is the coexistence seam: it routes an outbound
// WhatsApp message to the session transport when the per-tenant session
// flag is enabled and otherwise delegates to the primary (official)
// channel. Non-WhatsApp channels always use primary. Deny-by-default —
// any flag error or a disabled tenant falls through to primary, so the
// official channel is the safe default for every tenant.
type providerRoutingOutbound struct {
	primary inbox.OutboundChannel
	session inbox.OutboundChannel
	flag    wasessionchan.FeatureFlag
	logger  *slog.Logger
}

// SendMessage implements inbox.OutboundChannel.
func (r providerRoutingOutbound) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	if m.Channel == wasessionchan.Channel && r.session != nil && r.flag != nil {
		on, err := r.flag.Enabled(ctx, m.TenantID)
		if err == nil && on {
			return r.session.SendMessage(ctx, m)
		}
	}
	return r.primary.SendMessage(ctx, m)
}

// waSessionGloballyEnabled reports whether the global session kill-switch
// is on. Mirrors NewEnvFeatureFlag's globalOn predicate so the wire and
// the adapter agree on the meaning of the flag.
func waSessionGloballyEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	return strings.TrimSpace(getenv(wasessionchan.EnvSessionEnabled)) == "1"
}

// parseWASessionTenants parses FEATURE_WA_SESSION_TENANTS into the set of
// tenant ids whose sessions the Manager should start. Blank entries and
// malformed UUIDs are skipped (the same lenient parse the adapter's flag
// uses) and duplicates are collapsed so a session is started at most once
// per tenant.
func parseWASessionTenants(getenv func(string) string) []uuid.UUID {
	if getenv == nil {
		return nil
	}
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, raw := range strings.Split(getenv(wasessionchan.EnvSessionTenantAllow), ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
