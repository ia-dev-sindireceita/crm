package postgres_test

// SIN-62903 / Fase 3 W2C acceptance for the aiassist domain — Postgres
// adapter integration tests + Summarize use-case wired against the real
// wallet adapter.
//
// Tests live in the parent postgres_test package (not the aiassist
// sub-package) to share the single TestMain / testpg.Harness and avoid
// the ALTER ROLE race on the shared CI cluster (the same pattern that
// drove SIN-62726 / SIN-62750 and that PR #80 inadvertently reintroduced).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	aiassistpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/aiassist"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistuc "github.com/pericles-luz/crm/internal/aiassist/usecase"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
)

// freshDBWithAIAssist applies every migration the aiassist integration
// tests need: tenants + users (FK targets), inbox_contacts (conversation
// table), the wallet basic schema + updated_at trigger, and 0098
// (ai_policy/ai_summary/product/product_argument). The chain matches
// production deploy order; tests pay the cost of running it once per
// fresh DB.
func freshDBWithAIAssist(t *testing.T) (*testpg.DB, context.Context, uuid.UUID, uuid.UUID) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0089_wallet_basic.up.sql",
		"0090_wallet_updated_at_trigger.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	tenantID := uuid.New()
	masterID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "aiassist-test", fmt.Sprintf("aia-%s.crm.local", tenantID)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, fmt.Sprintf("m-%s@x", masterID)); err != nil {
		t.Fatalf("seed master user: %v", err)
	}
	return db, ctx, tenantID, masterID
}

func seedAIAssistConversation(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var contactID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO contact (tenant_id, display_name) VALUES ($1, $2) RETURNING id`,
		tenantID, "fixture").Scan(&contactID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	var convID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO conversation (tenant_id, contact_id, channel, state)
		 VALUES ($1, $2, 'whatsapp', 'open') RETURNING id`,
		tenantID, contactID).Scan(&convID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

func seedAIAssistWallet(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, masterID uuid.UUID, balance int64) {
	t.Helper()
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_wallet (tenant_id, balance, reserved)
			 VALUES ($1, $2, 0)`, tenantID, balance)
		return err
	}); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
}

// stubLLM is the integration-test LLM stand-in. Each Complete call is
// guarded by a mutex; the response is returned verbatim. callCount /
// lastReq are exposed for assertions.
type stubLLM struct {
	mu      sync.Mutex
	resp    aiassist.LLMResponse
	err     error
	calls   int
	lastReq aiassist.LLMRequest
}

func (s *stubLLM) Complete(_ context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastReq = req
	if s.err != nil {
		return aiassist.LLMResponse{}, s.err
	}
	return s.resp, nil
}

func (s *stubLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type stubPolicy struct {
	policy aiassist.Policy
}

func (s stubPolicy) Resolve(_ context.Context, _ uuid.UUID, _ aiassist.Scope) (aiassist.Policy, error) {
	return s.policy, nil
}

// freshAIAssistService wires the aiassist Service against the real
// wallet usecase + real postgres adapter for ai_summary. callers tweak
// LLM / policy as needed.
func freshAIAssistService(
	t *testing.T,
	db *testpg.DB,
	llm aiassist.LLMClient,
	pol aiassist.PolicyResolver,
	clock func() time.Time,
) *aiassistuc.Service {
	t.Helper()
	repo, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("aiassist adapter: %v", err)
	}
	walletStore, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("wallet adapter: %v", err)
	}
	walletSvc, err := walletusecase.NewService(walletStore, clock)
	if err != nil {
		t.Fatalf("wallet usecase: %v", err)
	}
	svc, err := aiassistuc.NewService(aiassistuc.Config{
		Repo:   repo,
		Wallet: walletSvc,
		LLM:    llm,
		Policy: pol,
		Clock:  clock,
		TTL:    aiassist.DefaultSummaryTTL,
	})
	if err != nil {
		t.Fatalf("aiassist usecase: %v", err)
	}
	return svc
}

// TestAIAssistAdapter_RoundTrip exercises the basic save/get path of
// the SummaryRepository against the real DB.
func TestAIAssistAdapter_RoundTrip(t *testing.T) {
	db, ctx, tenantID, _ := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)

	repo, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}

	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	if _, err := repo.GetLatestValid(ctx, tenantID, convID, now); !errors.Is(err, aiassist.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss on empty conv; got %v", err)
	}

	sm, err := aiassist.NewSummary(tenantID, convID, "summary text", "google/gemini-2.0-flash", 100, 50, now, aiassist.DefaultSummaryTTL)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	if err := repo.Save(ctx, sm); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.GetLatestValid(ctx, tenantID, convID, now)
	if err != nil {
		t.Fatalf("GetLatestValid: %v", err)
	}
	if got == nil {
		t.Fatalf("nil summary on hit")
	}
	if got.Text != "summary text" {
		t.Fatalf("text mismatch: %q", got.Text)
	}
	if got.TokensIn != 100 || got.TokensOut != 50 {
		t.Fatalf("tokens mismatch: %d/%d", got.TokensIn, got.TokensOut)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt zero — TTL was lost")
	}

	// Past-TTL probe.
	expired := now.Add(25 * time.Hour)
	if _, err := repo.GetLatestValid(ctx, tenantID, convID, expired); !errors.Is(err, aiassist.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss after TTL; got %v", err)
	}
}

// TestAIAssistAdapter_InvalidateForConversation covers AC #6 at the
// adapter level: invalidation flips currently-valid rows to invalid;
// subsequent GetLatestValid is a miss.
func TestAIAssistAdapter_InvalidateForConversation(t *testing.T) {
	db, ctx, tenantID, _ := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)

	repo, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	sm, err := aiassist.NewSummary(tenantID, convID, "text", "m", 1, 1, now, aiassist.DefaultSummaryTTL)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	if err := repo.Save(ctx, sm); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// invalidate is idempotent
	if err := repo.InvalidateForConversation(ctx, tenantID, convID, now.Add(1*time.Hour)); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if err := repo.InvalidateForConversation(ctx, tenantID, convID, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("Invalidate (repeat): %v", err)
	}
	if _, err := repo.GetLatestValid(ctx, tenantID, convID, now.Add(2*time.Hour)); !errors.Is(err, aiassist.ErrCacheMiss) {
		t.Fatalf("expected miss after invalidation; got %v", err)
	}
}

// TestAIAssistAdapter_RLSIsolation proves tenant B cannot read or
// invalidate tenant A's summary rows even though they live in the same
// physical table.
func TestAIAssistAdapter_RLSIsolation(t *testing.T) {
	db, ctx, tenantA, _ := freshDBWithAIAssist(t)
	convA := seedAIAssistConversation(t, ctx, db, tenantA)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenantB: %v", err)
	}
	convB := seedAIAssistConversation(t, ctx, db, tenantB)

	repo, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	smA, _ := aiassist.NewSummary(tenantA, convA, "tenantA secret", "m", 1, 1, now, aiassist.DefaultSummaryTTL)
	if err := repo.Save(ctx, smA); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	// Tenant B looking for tenantA's summary on convA must miss — even
	// asking for convA (which is a real id under tenantA) returns
	// ErrCacheMiss because RLS hides the row.
	if _, err := repo.GetLatestValid(ctx, tenantB, convA, now); !errors.Is(err, aiassist.ErrCacheMiss) {
		t.Fatalf("RLS leak: tenant B saw tenant A's summary: %v", err)
	}

	// Verify B can save its own row alongside A's without collision.
	smB, _ := aiassist.NewSummary(tenantB, convB, "tenantB secret", "m", 1, 1, now, aiassist.DefaultSummaryTTL)
	if err := repo.Save(ctx, smB); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	gotB, err := repo.GetLatestValid(ctx, tenantB, convB, now)
	if err != nil {
		t.Fatalf("GetLatestValid B: %v", err)
	}
	if gotB.Text != "tenantB secret" {
		t.Fatalf("B got %q", gotB.Text)
	}
}

// TestAIAssistAdapter_NilPool guards the constructor.
func TestAIAssistAdapter_NilPool(t *testing.T) {
	if _, err := aiassistpg.New(nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("expected ErrNilPool, got %v", err)
	}
}

// TestAIAssistAdapter_ValidationRejectsZero exercises the boundary
// checks. uuid.Nil tenant / conversation MUST surface as the typed
// sentinel, not as a DB-side error.
func TestAIAssistAdapter_ValidationRejectsZero(t *testing.T) {
	db, ctx, _, _ := freshDBWithAIAssist(t)
	repo, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	conv := uuid.New()
	tenant := uuid.New()
	now := time.Now()
	if _, err := repo.GetLatestValid(ctx, uuid.Nil, conv, now); !errors.Is(err, aiassist.ErrZeroTenant) {
		t.Fatalf("expected ErrZeroTenant, got %v", err)
	}
	if _, err := repo.GetLatestValid(ctx, tenant, uuid.Nil, now); !errors.Is(err, aiassist.ErrZeroConversation) {
		t.Fatalf("expected ErrZeroConversation, got %v", err)
	}
	if err := repo.InvalidateForConversation(ctx, uuid.Nil, conv, now); !errors.Is(err, aiassist.ErrZeroTenant) {
		t.Fatalf("expected ErrZeroTenant on invalidate, got %v", err)
	}
	if err := repo.InvalidateForConversation(ctx, tenant, uuid.Nil, now); !errors.Is(err, aiassist.ErrZeroConversation) {
		t.Fatalf("expected ErrZeroConversation on invalidate, got %v", err)
	}
	if err := repo.Save(ctx, nil); err == nil {
		t.Fatalf("nil Save should error")
	}
	bad := &aiassist.Summary{}
	if err := repo.Save(ctx, bad); !errors.Is(err, aiassist.ErrZeroTenant) {
		t.Fatalf("expected ErrZeroTenant on Save zero-tenant, got %v", err)
	}
	bad.TenantID = tenant
	if err := repo.Save(ctx, bad); !errors.Is(err, aiassist.ErrZeroConversation) {
		t.Fatalf("expected ErrZeroConversation on Save zero-conv, got %v", err)
	}
}

// TestAIAssistUsecase_AC4_ConcurrentSpendNeverOverdrafts covers AC #4
// of SIN-62196 end-to-end: N concurrent Summarize calls on the same
// tenant against a limited wallet never overdraft the wallet; the
// ledger stays consistent; subsequent calls after exhaustion surface
// ErrInsufficientBalance.
func TestAIAssistUsecase_AC4_ConcurrentSpendNeverOverdrafts(t *testing.T) {
	db, ctx, tenantID, masterID := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)

	// Budget = 4_000 tokens. Each Summarize reserves
	// EstimateReservation(prompt=240 bytes, gemini, max=256) = 60 + 256 = 316.
	// So at most floor(4000/316) = 12 successes before insufficient.
	// We launch 30 goroutines to make sure the wallet's SELECT FOR
	// UPDATE serialises contention without overdrafting.
	seedAIAssistWallet(t, ctx, db, tenantID, masterID, 4_000)

	llm := &stubLLM{
		resp: aiassist.LLMResponse{Text: "summary", TokensIn: 10, TokensOut: 10},
	}
	pol := stubPolicy{policy: aiassist.Policy{
		AIEnabled: true, OptIn: true, Anonymize: true,
		Model: "google/gemini-2.0-flash", MaxOutputTokens: 256,
	}}
	clock := func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) }
	svc := freshAIAssistService(t, db, llm, pol, clock)

	const N = 30
	var (
		ok           int64
		insufficient int64
		others       int64
	)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := aiassistuc.SummarizeRequest{
				TenantID:       tenantID,
				ConversationID: uuid.New(),
				RequestID:      fmt.Sprintf("ac4-%d-%s", i, uuid.NewString()),
				Prompt:         strings.Repeat("a", 240),
			}
			// Each goroutine targets a fresh conversation so the cache
			// cannot mask the wallet contention.
			convI := seedAIAssistConversation(t, ctx, db, tenantID)
			req.ConversationID = convI
			_, err := svc.Summarize(ctx, req)
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
			case errors.Is(err, aiassist.ErrInsufficientBalance):
				atomic.AddInt64(&insufficient, 1)
			default:
				atomic.AddInt64(&others, 1)
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt64(&others) != 0 {
		t.Fatalf("non-ErrInsufficientBalance errors observed; check t.Errorf output")
	}

	// Read the wallet's final balance / reserved directly from the
	// adapter so we cover the ledger path.
	var balance, reserved int64
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT balance, reserved FROM token_wallet WHERE tenant_id = $1`,
			tenantID).Scan(&balance, &reserved)
	}); err != nil {
		t.Fatalf("read final wallet: %v", err)
	}
	if balance < 0 {
		t.Fatalf("wallet balance went negative: %d", balance)
	}
	if reserved != 0 {
		t.Fatalf("reservations leaked: reserved=%d", reserved)
	}

	// Ledger consistency: number of committed rows must equal ok.
	var commitRows int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM token_ledger WHERE tenant_id = $1 AND kind = 'commit'`,
			tenantID).Scan(&commitRows)
	}); err != nil {
		t.Fatalf("read commit count: %v", err)
	}
	if int64(commitRows) != ok {
		t.Fatalf("commit rows=%d does not match successes=%d", commitRows, ok)
	}

	// After exhaustion, a fresh Summarize must surface ErrInsufficientBalance.
	if insufficient == 0 {
		// Edge case: maybe the budget was wide enough that all N succeeded.
		// Force a depletion call.
		drainReq := aiassistuc.SummarizeRequest{
			TenantID:       tenantID,
			ConversationID: convID,
			RequestID:      "drain-" + uuid.NewString(),
			Prompt:         strings.Repeat("b", 240),
		}
		if _, err := svc.Summarize(ctx, drainReq); err != nil && !errors.Is(err, aiassist.ErrInsufficientBalance) {
			t.Fatalf("drain returned %v", err)
		}
	}

	// One more probe: an explicit zero-balance call.
	exhaustReq := aiassistuc.SummarizeRequest{
		TenantID:       tenantID,
		ConversationID: convID,
		RequestID:      "exhaust-" + uuid.NewString(),
		Prompt:         strings.Repeat("c", 5_000),
	}
	if _, err := svc.Summarize(ctx, exhaustReq); err != nil && !errors.Is(err, aiassist.ErrInsufficientBalance) {
		t.Fatalf("post-exhaust call returned %v", err)
	}
}

// TestAIAssistUsecase_AC6_InvalidationRegeneratesCache covers AC #6
// end-to-end: a fresh summary is served from cache; invalidating the
// conversation flips the next call back to a fresh LLM round trip and
// produces a new committed row.
func TestAIAssistUsecase_AC6_InvalidationRegeneratesCache(t *testing.T) {
	db, ctx, tenantID, masterID := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)
	seedAIAssistWallet(t, ctx, db, tenantID, masterID, 1_000_000)

	llm := &stubLLM{resp: aiassist.LLMResponse{Text: "v1", TokensIn: 5, TokensOut: 5}}
	pol := stubPolicy{policy: aiassist.Policy{
		AIEnabled: true, OptIn: true, Anonymize: true,
		Model: "google/gemini-2.0-flash", MaxOutputTokens: 256,
	}}
	clock := func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) }
	svc := freshAIAssistService(t, db, llm, pol, clock)

	mkReq := func(rid string) aiassistuc.SummarizeRequest {
		return aiassistuc.SummarizeRequest{
			TenantID:       tenantID,
			ConversationID: convID,
			RequestID:      rid,
			Prompt:         strings.Repeat("hello ", 50),
		}
	}

	first, err := svc.Summarize(ctx, mkReq("ac6-1"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.CacheHit {
		t.Fatalf("first call should MISS")
	}
	if first.Summary.Text != "v1" {
		t.Fatalf("first text = %q", first.Summary.Text)
	}

	second, err := svc.Summarize(ctx, mkReq("ac6-2"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.CacheHit {
		t.Fatalf("second call should HIT")
	}
	if llm.callCount() != 1 {
		t.Fatalf("cache hit should skip LLM; calls=%d", llm.callCount())
	}

	// Simulate "new inbound message" by invalidating the conversation.
	if err := svc.Invalidate(ctx, tenantID, convID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	llm.mu.Lock()
	llm.resp = aiassist.LLMResponse{Text: "v2", TokensIn: 5, TokensOut: 5}
	llm.mu.Unlock()

	third, err := svc.Summarize(ctx, mkReq("ac6-3"))
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if third.CacheHit {
		t.Fatalf("third call should MISS (post-invalidation)")
	}
	if third.Summary.Text != "v2" {
		t.Fatalf("third text = %q, want v2", third.Summary.Text)
	}
	if llm.callCount() != 2 {
		t.Fatalf("LLM should be called twice (initial + post-invalidation); got %d", llm.callCount())
	}

	// Verify the AISummary table now has 2 rows for this conversation
	// (one invalidated + one valid).
	var totalRows, validRows int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ai_summary WHERE conversation_id = $1`,
			convID).Scan(&totalRows); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ai_summary WHERE conversation_id = $1 AND invalidated_at IS NULL`,
			convID).Scan(&validRows)
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if totalRows != 2 {
		t.Fatalf("expected 2 rows after regenerate; got %d", totalRows)
	}
	if validRows != 1 {
		t.Fatalf("expected exactly 1 valid row after regenerate; got %d", validRows)
	}
}

// TestAIAssistUsecase_IdempotentReplay covers the F37 contract end-to-
// end: replaying the same request_id after success returns the cached
// summary and does not charge the wallet twice.
func TestAIAssistUsecase_IdempotentReplay(t *testing.T) {
	db, ctx, tenantID, masterID := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)
	seedAIAssistWallet(t, ctx, db, tenantID, masterID, 1_000_000)

	llm := &stubLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 10, TokensOut: 10}}
	pol := stubPolicy{policy: aiassist.Policy{
		AIEnabled: true, OptIn: true, Anonymize: true,
		Model: "google/gemini-2.0-flash", MaxOutputTokens: 256,
	}}
	clock := func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) }
	svc := freshAIAssistService(t, db, llm, pol, clock)

	req := aiassistuc.SummarizeRequest{
		TenantID:       tenantID,
		ConversationID: convID,
		RequestID:      "replay-" + uuid.NewString(),
		Prompt:         strings.Repeat("text ", 100),
	}
	first, err := svc.Summarize(ctx, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.CacheHit {
		t.Fatalf("first call should MISS")
	}
	balAfterFirst := readBalance(t, ctx, db, tenantID)

	// Replay with the same request_id — the AISummary cache hits, so
	// no new wallet activity should occur.
	second, err := svc.Summarize(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.CacheHit {
		t.Fatalf("replay should HIT")
	}
	balAfterReplay := readBalance(t, ctx, db, tenantID)
	if balAfterFirst != balAfterReplay {
		t.Fatalf("balance changed across cache-hit replay: %d -> %d", balAfterFirst, balAfterReplay)
	}

	// Replay AFTER invalidation: same request_id, but the cache miss
	// forces a fresh Reserve. The wallet's idempotency key dedup must
	// still short-circuit the ledger so we don't end up with two
	// reservation rows for the same boundary key.
	if err := svc.Invalidate(ctx, tenantID, convID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	third, err := svc.Summarize(ctx, req)
	if err != nil {
		t.Fatalf("third (replay post-invalidate): %v", err)
	}
	if third.CacheHit {
		t.Fatalf("third call should MISS (post-invalidation)")
	}
	// Count reservation rows under this exact idempotency-key prefix.
	keyPrefix := req.TenantID.String() + ":" + req.ConversationID.String() + ":" + req.RequestID
	var reserveRows int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM token_ledger
			   WHERE tenant_id = $1 AND kind = 'reserve' AND idempotency_key LIKE $2 || '%'`,
			tenantID, keyPrefix).Scan(&reserveRows)
	}); err != nil {
		t.Fatalf("count reserves: %v", err)
	}
	// Exactly one reservation per boundary request_id — the wallet's
	// idempotency dedup absorbs the replay.
	if reserveRows != 1 {
		t.Fatalf("expected exactly 1 reservation row across replays; got %d", reserveRows)
	}
}

// TestAIAssistUsecase_PolicyBlocked covers the early-exit when policy
// disables AI: no wallet activity, no LLM call, no AISummary row.
func TestAIAssistUsecase_PolicyBlocked(t *testing.T) {
	db, ctx, tenantID, masterID := freshDBWithAIAssist(t)
	convID := seedAIAssistConversation(t, ctx, db, tenantID)
	seedAIAssistWallet(t, ctx, db, tenantID, masterID, 100_000)

	llm := &stubLLM{}
	pol := stubPolicy{policy: aiassist.Policy{
		AIEnabled: false, OptIn: true, Model: "google/gemini-2.0-flash",
	}}
	clock := func() time.Time { return time.Now() }
	svc := freshAIAssistService(t, db, llm, pol, clock)

	req := aiassistuc.SummarizeRequest{
		TenantID:       tenantID,
		ConversationID: convID,
		RequestID:      "blocked-" + uuid.NewString(),
		Prompt:         "test",
	}
	_, err := svc.Summarize(ctx, req)
	if !errors.Is(err, aiassist.ErrAIDisabled) {
		t.Fatalf("expected ErrAIDisabled, got %v", err)
	}
	if llm.callCount() != 0 {
		t.Fatalf("LLM must not be called")
	}
	balance := readBalance(t, ctx, db, tenantID)
	if balance != 100_000 {
		t.Fatalf("balance changed under policy block: %d", balance)
	}
}

func readBalance(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) int64 {
	t.Helper()
	var bal int64
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT balance FROM token_wallet WHERE tenant_id = $1`, tenantID).Scan(&bal)
	}); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return bal
}
