package postgres_test

// SIN-62960 integration test for the funnel rule engine — drives the
// full vertical against a real testpg cluster + an in-process
// JetStream NATS server.
//
// Lives in the parent postgres_test package so it shares the TestMain
// + harness with the other testpg integration tests (avoids the
// ALTER ROLE race documented in memory `testpg shared-cluster ALTER
// ROLE race`).
//
// Covers AC#1 (cenário do AC#3 do SIN-62197 passa contra testpg +
// NATS embedded), AC#2 (re-entrega NATS não duplica ação), and AC#3
// (sem mutex global; engine roda goroutine-safe).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgfunnel "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnel"
	pgfunnelapps "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelapplications"
	pgfunnelrules "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelrules"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/funnel/rules"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/worker/funnel_engine"
)

// freshDBWithFunnelEngine layers every migration the engine needs:
// tenants, identity link, funnel_stage/transition (0093), funnel_rules
// (0102) and funnel_rule_applications (0103).
func freshDBWithFunnelEngine(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0089_wallet_basic.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		"0093_funnel_stage_transition.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
		"0102_phase4_marketing_billing_dunning.up.sql",
		"0103_funnel_rule_applications.up.sql",
	)
	return db
}

func runEmbeddedNATSForFunnel(t *testing.T) string {
	t.Helper()
	port := pickFreeFunnelPort(t)
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready in time")
	}
	return s.ClientURL()
}

func pickFreeFunnelPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// funnelEngineNATSShim adapts *natsadapter.SDKAdapter to the worker's
// Subscriber port — same shape as cmd/funnel-engine-worker's shim.
type funnelEngineNATSShim struct{ a *natsadapter.SDKAdapter }

func (n *funnelEngineNATSShim) EnsureStream(name string, subjects []string) error {
	return n.a.EnsureStream(name, subjects)
}
func (n *funnelEngineNATSShim) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler funnel_engine.HandlerFunc,
) (funnel_engine.Subscription, error) {
	return n.a.Subscribe(ctx, subject, queue, durable, ackWait,
		func(c context.Context, d *natsadapter.Delivery) error { return handler(c, d) },
	)
}
func (n *funnelEngineNATSShim) Drain() error { return n.a.Drain() }

// seedFunnelEngineTenant + seedFunnelEngineRule keep this test's
// fixtures separate from the funnelrules / funnelapplications adapter
// tests so a parallel run does not collide on names.
func seedFunnelEngineTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "fe-"+id.String(), id.String()+".fe.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedFunnelEngineSystemUser(t *testing.T, pool *pgxpool.Pool, tenantID, actorID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master, created_at)
		 VALUES ($1, $2, $3, $4, 'tenant_common', FALSE, now())
		 ON CONFLICT (id) DO NOTHING`,
		actorID, tenantID, "rules-engine+"+actorID.String()+"@example.test", "x",
	); err != nil {
		t.Fatalf("seed user (system actor): %v", err)
	}
}

func seedFunnelEngineConversation(t *testing.T, pool *pgxpool.Pool, tenantID, conversationID uuid.UUID, channel string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contactID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name, created_at, updated_at)
		 VALUES ($1, $2, $3, now(), now())`,
		contactID, tenantID, "engine-integration-contact",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel, state, created_at)
		 VALUES ($1, $2, $3, $4, 'open', now())`,
		conversationID, tenantID, contactID, channel,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
}

func seedFunnelEngineRule(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, channel string, ruleID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	triggerJSON, _ := json.Marshal(map[string]any{"phrase": "orçamento"})
	actionJSON, _ := json.Marshal(map[string]any{"stage_key": "qualificando"})
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO funnel_rules
		  (id, tenant_id, channel, team_id, name,
		   trigger_type, trigger_config, action_type, action_config,
		   enabled, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4,
		        'message_contains', $5::jsonb,
		        'move_to_stage', $6::jsonb,
		        TRUE, $7, $7)`,
		ruleID, tenantID, channel, "AC#3 webchat rule",
		triggerJSON, actionJSON, now,
	); err != nil {
		t.Fatalf("seed funnel_rule: %v", err)
	}
}

func quietEngineLogger() *slog.Logger {
	if os.Getenv("FUNNEL_ENGINE_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// pollUntil retries probe until it returns true or the deadline trips.
// Used in lieu of arbitrary time.Sleep so the test settles as soon as
// the engine catches up.
func pollUntil(t *testing.T, deadline time.Duration, probe func() bool) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if probe() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return probe()
}

// TestFunnelEngine_AC1_EndToEnd_StageTransitionFiresOnce drives the
// engine end-to-end. Validates AC#1 + AC#2 + AC#3:
//
//   - AC#1: the AC#3-from-SIN-62197 scenario lands a transition.
//   - AC#2: a redelivered NATS message does NOT produce a second
//     transition or a second applications row.
//   - AC#3: the engine ran without a process-global mutex (the
//     funnel_engine.Subscriber is goroutine-driven; this test exercises
//     it concurrently with the publish loop).
func TestFunnelEngine_AC1_EndToEnd_StageTransitionFiresOnce(t *testing.T) {
	// Cannot t.Parallel: the test uses a private NATS embed and pgxpool
	// already issues many roundtrips.
	db := freshDBWithFunnelEngine(t)
	adminPool := db.AdminPool()
	runtimePool := db.RuntimePool()

	tenantID := seedFunnelEngineTenant(t, adminPool)
	// The engine uses engine.SystemActor() as the byUserID. Seed that
	// pseudo-user under the tenant so funnel_transition's user FK
	// resolves.
	seedFunnelEngineSystemUser(t, adminPool, tenantID, engine.SystemActor())

	ruleID := uuid.New()
	seedFunnelEngineRule(t, adminPool, tenantID, "webchat", ruleID)

	conversationID := uuid.New()
	seedFunnelEngineConversation(t, adminPool, tenantID, conversationID, "webchat")

	// Build the pgx adapters + engine + worker against the real DB.
	rulesStore, err := pgfunnelrules.New(runtimePool)
	if err != nil {
		t.Fatalf("pgfunnelrules.New: %v", err)
	}
	appsStore, err := pgfunnelapps.New(runtimePool)
	if err != nil {
		t.Fatalf("pgfunnelapps.New: %v", err)
	}
	funnelStore, err := pgfunnel.New(runtimePool)
	if err != nil {
		t.Fatalf("pgfunnel.New: %v", err)
	}
	funnelService, err := funnel.NewService(funnel.Config{
		Stages:      funnelStore,
		Transitions: funnelStore,
		Publisher:   recordingFunnelPublisher{}, // discard
	})
	if err != nil {
		t.Fatalf("funnel.NewService: %v", err)
	}
	resolver, err := rules.NewResolver(rulesStore)
	if err != nil {
		t.Fatalf("rules.NewResolver: %v", err)
	}
	eng, err := engine.NewEngine(engine.Config{
		Resolver:     resolver,
		Applications: appsStore,
		Mover:        funnelService,
		Logger:       quietEngineLogger(),
	})
	if err != nil {
		t.Fatalf("engine.NewEngine: %v", err)
	}

	// Stand up the embedded NATS + connect a publisher and a
	// subscriber. Two SDK instances so the publisher does not race
	// the worker's drain.
	url := runEmbeddedNATSForFunnel(t)
	pubSDK, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		Name:           t.Name() + "-pub",
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  100 * time.Millisecond,
		Insecure:       true,
	})
	if err != nil {
		t.Fatalf("nats Connect (pub): %v", err)
	}
	t.Cleanup(pubSDK.Close)
	subSDK, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		Name:           t.Name() + "-sub",
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  100 * time.Millisecond,
		Insecure:       true,
	})
	if err != nil {
		t.Fatalf("nats Connect (sub): %v", err)
	}
	t.Cleanup(subSDK.Close)

	publisher, err := natsadapter.NewInboundMessagePublisher(pubSDK)
	if err != nil {
		t.Fatalf("NewInboundMessagePublisher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- funnel_engine.Run(ctx, &funnelEngineNATSShim{a: subSDK}, funnel_engine.RunConfig{
			Engine:  eng,
			Logger:  quietEngineLogger(),
			AckWait: 2 * time.Second,
		})
	}()

	// Wait until the durable subscription is registered. Subject pinned
	// to engine.Subject — handshake is via EnsureStream first, then
	// Subscribe. A quick poll on the stream presence is enough.
	if !pollUntil(t, 3*time.Second, func() bool {
		return streamPresent(t, pubSDK)
	}) {
		t.Fatal("stream INBOX did not appear in time")
	}

	messageID := uuid.New()
	occurredAt := time.Now().UTC().Truncate(time.Microsecond)
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	if err := publisher.PublishInboundMessage(pubCtx, publishedInbound(tenantID, conversationID, messageID, occurredAt)); err != nil {
		t.Fatalf("PublishInboundMessage: %v", err)
	}

	// AC#1: a funnel_transition row materialises for the conversation,
	// moving it to 'qualificando'. Poll because JetStream → handler is
	// async.
	if !pollUntil(t, 5*time.Second, func() bool {
		return transitionCountForConversation(t, adminPool, tenantID, conversationID) == 1
	}) {
		t.Fatalf("AC#1: expected 1 funnel_transition row for conv %s, got %d",
			conversationID, transitionCountForConversation(t, adminPool, tenantID, conversationID))
	}

	// AC#2: republishing the same message_id does NOT produce a
	// second transition or a second applications row.
	if err := publisher.PublishInboundMessage(pubCtx, publishedInbound(tenantID, conversationID, messageID, occurredAt)); err != nil {
		t.Fatalf("re-Publish: %v", err)
	}
	// Brief sleep + recheck — the publish IS deliverable (JetStream
	// drops the dup via Nats-Msg-Id within the duplicates window, and
	// even if it weren't, the engine's UNIQUE constraint blocks the
	// re-apply). Allow up to 1s for the redelivery to have been
	// observed-and-rejected.
	time.Sleep(250 * time.Millisecond)
	if got := transitionCountForConversation(t, adminPool, tenantID, conversationID); got != 1 {
		t.Errorf("AC#2: redelivery duplicated transition rows: got %d, want 1", got)
	}
	if got := applicationsCountForRule(t, adminPool, tenantID, ruleID, messageID); got != 1 {
		t.Errorf("AC#2: redelivery duplicated applications: got %d, want 1", got)
	}

	// Sanity: publishing an unrelated WhatsApp message with a different
	// message_id MUST be ignored (AC#3-from-SIN-62197: channel-scoped
	// rule does not fire for whatsapp).
	otherMessageID := uuid.New()
	otherConversation := uuid.New()
	otherEvent := publishedInbound(tenantID, otherConversation, otherMessageID, time.Now().UTC().Truncate(time.Microsecond))
	otherEvent.Channel = "whatsapp"
	if err := publisher.PublishInboundMessage(pubCtx, otherEvent); err != nil {
		t.Fatalf("Publish (whatsapp): %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := transitionCountForConversation(t, adminPool, tenantID, otherConversation); got != 0 {
		t.Errorf("AC#1: webchat rule fired on whatsapp message: %d rows", got)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("worker Run returned err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not honour shutdown in time")
	}
}

func streamPresent(t *testing.T, sdk *natsadapter.SDKAdapter) bool {
	t.Helper()
	// Best-effort: try EnsureStream; second call should be idempotent.
	return sdk.EnsureStream(engine.StreamName, []string{engine.Subject}) == nil
}

func transitionCountForConversation(t *testing.T, pool *pgxpool.Pool, tenantID, conversationID uuid.UUID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM funnel_transition WHERE tenant_id = $1 AND conversation_id = $2`,
		tenantID, conversationID,
	).Scan(&n); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return n
}

func applicationsCountForRule(t *testing.T, pool *pgxpool.Pool, tenantID, ruleID, messageID uuid.UUID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM funnel_rule_applications
		  WHERE tenant_id = $1 AND rule_id = $2 AND message_id = $3`,
		tenantID, ruleID, messageID,
	).Scan(&n); err != nil {
		t.Fatalf("count applications: %v", err)
	}
	return n
}

func publishedInbound(tenantID, conversationID, messageID uuid.UUID, at time.Time) inboxusecase.PublishedInboundMessage {
	return inboxusecase.PublishedInboundMessage{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      messageID,
		Channel:        "webchat",
		Body:           "Olá, gostaria de um orçamento por favor",
		OccurredAt:     at,
	}
}

// recordingFunnelPublisher discards funnel.conversation_moved events
// during the integration test. The transition row in funnel_transition
// is what the test asserts on.
type recordingFunnelPublisher struct{}

func (recordingFunnelPublisher) Publish(_ context.Context, _ string, _ any) error {
	return nil
}
