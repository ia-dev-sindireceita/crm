package main

// SIN-65244 — operator AI-assist Summarizer wireup tests.
//
// Coverage:
//   - soft-degrade gate: no OPENROUTER_API_KEY → (nil, nil), boot OK,
//     pool never touched (so passing a nil pool is safe).
//   - default-model decorator: empty request model is filled with the
//     env-resolved default; a non-empty model passes through untouched.
//   - assembleAIAssistSummarizer: builds a real *aiassistusecase.Service
//     from in-memory port fakes (no Postgres), and surfaces the
//     NewService validation errors.
//   - route registration: when the inbox handler is wired with a
//     Summarizer the POST /inbox/conversations/{id}/ai-assist route is
//     registered; when nil it is absent (the activation contract the
//     soft-degrade relies on).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/wallet"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// --- soft-degrade gate ---------------------------------------------------

func TestBuildAIAssistSummarizer_SoftDegradesWithoutKey(t *testing.T) {
	t.Parallel()
	// Passing a nil pool proves the builder returns before touching
	// storage when the key is absent — the soft-degrade path must not
	// dereference the pool.
	got, err := buildAIAssistSummarizerFromPool(nil, func(string) string { return "" })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("Summarizer = %T; want nil (feature off without OPENROUTER_API_KEY)", got)
	}
}

func TestBuildAIAssistSummarizer_SoftDegradesNilGetenv(t *testing.T) {
	t.Parallel()
	got, err := buildAIAssistSummarizerFromPool(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("Summarizer = %T; want nil for nil getenv", got)
	}
}

// --- default-model decorator ---------------------------------------------

type capturingLLM struct {
	gotModel string
}

func (c *capturingLLM) Complete(_ context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	c.gotModel = req.Model
	return aiassist.LLMResponse{Text: "ok"}, nil
}

func TestDefaultModelLLMClient_FillsEmptyModel(t *testing.T) {
	t.Parallel()
	inner := &capturingLLM{}
	d := defaultModelLLMClient{inner: inner, model: "x-ai/grok"}
	if _, err := d.Complete(context.Background(), aiassist.LLMRequest{Model: ""}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if inner.gotModel != "x-ai/grok" {
		t.Fatalf("forwarded model = %q; want x-ai/grok (empty must be filled)", inner.gotModel)
	}
}

func TestDefaultModelLLMClient_PassesThroughExplicitModel(t *testing.T) {
	t.Parallel()
	inner := &capturingLLM{}
	d := defaultModelLLMClient{inner: inner, model: "x-ai/grok"}
	if _, err := d.Complete(context.Background(), aiassist.LLMRequest{Model: "anthropic/claude-haiku-4.5"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if inner.gotModel != "anthropic/claude-haiku-4.5" {
		t.Fatalf("forwarded model = %q; want the explicit per-request model untouched", inner.gotModel)
	}
}

func TestDefaultModelLLMClient_BlankModelTreatedAsEmpty(t *testing.T) {
	t.Parallel()
	inner := &capturingLLM{}
	d := defaultModelLLMClient{inner: inner, model: "x-ai/grok"}
	if _, err := d.Complete(context.Background(), aiassist.LLMRequest{Model: "   "}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if inner.gotModel != "x-ai/grok" {
		t.Fatalf("forwarded model = %q; want x-ai/grok (whitespace-only must be filled)", inner.gotModel)
	}
}

func TestBuildAIAssistLLMClient(t *testing.T) {
	t.Parallel()
	// A non-empty key constructs the openrouter client → shim → default
	// model decorator successfully.
	llm, err := buildAIAssistLLMClient("sk-test-fixture", "x-ai/grok")
	if err != nil {
		t.Fatalf("buildAIAssistLLMClient: %v", err)
	}
	d, ok := llm.(defaultModelLLMClient)
	if !ok {
		t.Fatalf("got %T; want defaultModelLLMClient", llm)
	}
	if d.model != "x-ai/grok" {
		t.Fatalf("decorator model = %q; want x-ai/grok", d.model)
	}
	// An empty key is rejected by the underlying openrouter client
	// constructor (defence in depth — the caller already gates on it).
	if _, err := buildAIAssistLLMClient("", "x-ai/grok"); err == nil {
		t.Fatal("expected error for empty API key, got nil")
	}
}

// --- assembleAIAssistSummarizer (in-memory port fakes) -------------------
//
// NewService validates required deps and the consent trio but never
// calls any of these at construction time, so the fakes are no-ops.

type fakeSummaryRepo struct{}

func (fakeSummaryRepo) GetLatestValid(context.Context, uuid.UUID, uuid.UUID, time.Time) (*aiassist.Summary, error) {
	return nil, aiassist.ErrCacheMiss
}
func (fakeSummaryRepo) Save(context.Context, *aiassist.Summary) error { return nil }
func (fakeSummaryRepo) InvalidateForConversation(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

type fakeWallet struct{}

func (fakeWallet) Reserve(context.Context, uuid.UUID, int64, string) (*wallet.Reservation, error) {
	return &wallet.Reservation{}, nil
}
func (fakeWallet) Commit(context.Context, *wallet.Reservation, int64, string) error { return nil }
func (fakeWallet) Release(context.Context, *wallet.Reservation, string) error       { return nil }

type fakePolicy struct{}

func (fakePolicy) Resolve(context.Context, uuid.UUID, aiassist.Scope) (aiassist.Policy, error) {
	return aiassist.Policy{}, nil
}

type fakeConsent struct{}

func (fakeConsent) HasConsent(context.Context, aipolicy.ConsentScope, string, string) (bool, error) {
	return true, nil
}

type fakeAnonymizer struct{}

func (fakeAnonymizer) Anonymize(_ context.Context, text string) (string, error) { return text, nil }

func TestAssembleAIAssistSummarizer_BuildsService(t *testing.T) {
	t.Parallel()
	got, err := assembleAIAssistSummarizer(aiAssistSummarizerDeps{
		Repo:              fakeSummaryRepo{},
		Wallet:            fakeWallet{},
		Policy:            fakePolicy{},
		LLM:               &capturingLLM{},
		Consent:           fakeConsent{},
		Anonymizer:        fakeAnonymizer{},
		AnonymizerVersion: "v1",
	})
	if err != nil {
		t.Fatalf("assembleAIAssistSummarizer: %v", err)
	}
	if got == nil {
		t.Fatal("Summarizer is nil; want a wired *Service")
	}
	if _, ok := got.(*aiassistusecase.Service); !ok {
		t.Fatalf("got %T; want *aiassistusecase.Service", got)
	}
}

func TestAssembleAIAssistSummarizer_SurfacesMissingDep(t *testing.T) {
	t.Parallel()
	// LLM nil is a required-dep violation NewService rejects.
	_, err := assembleAIAssistSummarizer(aiAssistSummarizerDeps{
		Repo:   fakeSummaryRepo{},
		Wallet: fakeWallet{},
		Policy: fakePolicy{},
		LLM:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil LLM, got nil")
	}
}

// --- route registration contract -----------------------------------------

type fakeSummarizer struct{}

func (fakeSummarizer) Summarize(context.Context, aiassistusecase.SummarizeRequest) (*aiassistusecase.SummarizeResponse, error) {
	return &aiassistusecase.SummarizeResponse{}, nil
}

func TestAssembleInboxHandler_RegistersAIAssistRouteWhenSummarizerSet(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	handler, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		ReplyDelay: 0,
		AIAssist:   webinbox.AssistDeps{Summarizer: fakeSummarizer{}},
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	url := srv.URL + "/inbox/conversations/" + uuid.NewString() + "/ai-assist"
	res, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST ai-assist: %v", err)
	}
	defer res.Body.Close()
	// A registered route runs the handler (which 500s on the missing
	// tenancy context the chi auth stack would inject in production) —
	// anything but 404 proves the route is mounted.
	if res.StatusCode == http.StatusNotFound {
		t.Fatal("POST ai-assist returned 404; route not registered despite Summarizer set")
	}
}

func TestAssembleInboxHandler_OmitsAIAssistRouteWhenSummarizerNil(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	handler, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		ReplyDelay: 0,
		// AIAssist left zero → Summarizer nil → route must NOT mount.
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	url := srv.URL + "/inbox/conversations/" + uuid.NewString() + "/ai-assist"
	res, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST ai-assist: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST ai-assist returned %d; want 404 (route must be absent without a Summarizer)", res.StatusCode)
	}
}
