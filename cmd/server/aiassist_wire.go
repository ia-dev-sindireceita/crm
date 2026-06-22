package main

// SIN-65244 — operator AI-assist Summarizer wireup.
//
// The web/inbox handler auto-registers POST /inbox/conversations/{id}/
// ai-assist (and renders the "Resumir + sugerir" button) ONLY when
// webinbox.Deps.AIAssist.Summarizer != nil. Until this file, nothing
// in cmd/server ever constructed that Summarizer, so in production the
// route never mounted and the button never rendered. This wire closes
// that gap by assembling the internal/aiassist orchestrator at boot
// and injecting it into the llmcustomer inbox handler.
//
// SOFT activation gate (deliberately distinct from the persona's HARD
// gate). The persona path (persona_llm_provider_wire.go) aborts boot
// when PERSONA_LLM_PROVIDER=openrouter without OPENROUTER_API_KEY — a
// misconfigured persona must fail loud. AI-assist is the opposite: it
// is an opt-in operator convenience, so a missing OPENROUTER_API_KEY
// must NOT down the listener. Instead the Summarizer stays nil, the
// inbox handler simply does not register the ai-assist route or render
// the button, and the rest of /inbox keeps working. Enabling the
// feature is purely "set OPENROUTER_API_KEY (+ optionally
// AIASSIST_LLM_MODEL) on the VPS and recreate the container" — no code
// change, and rollback is "remove the key". The key is never logged.
//
// Hexagonal lens: the Summarizer's collaborators are all ports. The
// pure assembler (assembleAIAssistSummarizer) takes the already-built
// ports so it is unit-testable with in-memory fakes; the production
// wrapper (buildAIAssistSummarizerFromPool) is the only code that
// touches Postgres / the OpenRouter SDK.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	openrouterclient "github.com/pericles-luz/crm/adapters/openrouter"
	aiassistadapter "github.com/pericles-luz/crm/internal/adapter/aiassist"
	aiassistpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/aiassist"
	aipolicypg "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	walletpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	openrouterllm "github.com/pericles-luz/crm/internal/adapter/llm/openrouter"
	regexanon "github.com/pericles-luz/crm/internal/ai-assist/anonymizer/regex"
	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/aipolicy"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// defaultModelLLMClient decorates an aiassist.LLMClient so a request
// that carries no explicit model (the common case — the policy row
// left ai_policy.model blank) routes to the env-resolved default
// instead of the adapter's hardcoded const. This is the seam where the
// unified OPENROUTER_MODEL / AIASSIST_LLM_MODEL knob (SIN-65244) takes
// effect for the operator AI-assist point: a non-empty per-request /
// per-policy model still wins, but the fallback is now configurable.
type defaultModelLLMClient struct {
	inner aiassist.LLMClient
	model string
}

var _ aiassist.LLMClient = defaultModelLLMClient{}

// Complete fills req.Model with the configured default when the caller
// left it blank, then delegates. The decorator never logs the prompt
// and never inspects the API key — secret handling stays in the
// underlying adapter.
func (d defaultModelLLMClient) Complete(ctx context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	if strings.TrimSpace(req.Model) == "" {
		req.Model = d.model
	}
	return d.inner.Complete(ctx, req)
}

// newAIAssistDeps bundles the assembled summarizer into the inbox
// AssistDeps. The production summarizer is a *aiassistusecase.Service,
// which also satisfies webinbox.AssistSummaryReader (the SIN-65474
// staleness read side), so we surface it through SummaryReader via a
// type assertion. Keeping the assertion here (rather than changing the
// existing builder signatures) leaves the SIN-65244 wire tests
// untouched. A nil summarizer (soft-degrade) yields the assertion ok ==
// false, so both fields stay nil and the feature stays off.
func newAIAssistDeps(summarizer webinbox.AssistSummarizer) webinbox.AssistDeps {
	deps := webinbox.AssistDeps{Summarizer: summarizer}
	if reader, ok := summarizer.(webinbox.AssistSummaryReader); ok {
		deps.SummaryReader = reader
	}
	return deps
}

// buildAIAssistLLMClient constructs the OpenRouter-backed
// aiassist.LLMClient: the validated SDK client → the W2C shim → the
// env-default-model decorator. apiKey is required (the caller has
// already gated on it); model is the resolved AI-assist default.
// aiAssistLLMTimeout is the per-call deadline for the operator
// summarizer. Summarisation prompts are larger than persona prompts
// (full conversation transcript) and the model (gemini-2.5-flash or
// configured override) can take 30–60 s on long threads. 8 s (the
// adapter default) is too tight and causes context-deadline errors.
const aiAssistLLMTimeout = 90 * time.Second

func buildAIAssistLLMClient(apiKey, model string) (aiassist.LLMClient, error) {
	client, err := openrouterclient.New(openrouterclient.Config{
		APIKey:  apiKey,
		Timeout: aiAssistLLMTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("openrouter client: %w", err)
	}
	return defaultModelLLMClient{
		inner: openrouterllm.New(client),
		model: model,
	}, nil
}

// aiAssistSummarizerDeps bundles the ports assembleAIAssistSummarizer
// needs. Splitting it out from the pool-backed builder keeps the
// production assembly and the in-memory test assembly on the same
// code path.
type aiAssistSummarizerDeps struct {
	Repo              aiassist.SummaryRepository
	Wallet            aiassist.WalletClient
	Policy            aiassist.PolicyResolver
	LLM               aiassist.LLMClient
	Consent           aiassist.ConsentService
	Anonymizer        aiassist.Anonymizer
	AnonymizerVersion string
}

// assembleAIAssistSummarizer is the pure wireup: given already-built
// ports, return the webinbox.AssistSummarizer the inbox handler
// activates on. The consent gate (Consent + Anonymizer +
// AnonymizerVersion) is wired together so production calls run the
// SIN-62928 LGPD gate — the use case rejects the trio being partially
// set, so all three travel together here.
func assembleAIAssistSummarizer(deps aiAssistSummarizerDeps) (webinbox.AssistSummarizer, error) {
	svc, err := aiassistusecase.NewService(aiassistusecase.Config{
		Repo:              deps.Repo,
		Wallet:            deps.Wallet,
		LLM:               deps.LLM,
		Policy:            deps.Policy,
		TTL:               aiassist.DefaultSummaryTTL,
		Clock:             time.Now,
		Consent:           deps.Consent,
		Anonymizer:        deps.Anonymizer,
		AnonymizerVersion: deps.AnonymizerVersion,
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// buildAIAssistSummarizerFromPool builds the production Summarizer from
// a runtime pool. The SOFT activation gate returns (nil, nil) — feature
// off, boot continues — when OPENROUTER_API_KEY is unset/blank. Any
// other return value with a nil error is a wired Summarizer; a non-nil
// error signals a genuine wiring fault the caller logs and degrades on
// (it must NOT down the inbox).
func buildAIAssistSummarizerFromPool(pool *pgxpool.Pool, getenv func(string) string) (webinbox.AssistSummarizer, error) {
	key := ""
	if getenv != nil {
		key = strings.TrimSpace(getenv(envOpenRouterAPIKey))
	}
	if key == "" {
		// Soft-degrade: never log the (absent) key, never fail boot.
		log.Printf("crm: ai-assist operator summarizer disabled — OPENROUTER_API_KEY unset (soft-degrade; route + button stay off)")
		return nil, nil
	}

	llm, err := buildAIAssistLLMClient(key, ReadAIAssistModel(getenv))
	if err != nil {
		return nil, fmt.Errorf("ai-assist llm: %w", err)
	}
	repo, err := aiassistpg.New(pool)
	if err != nil {
		return nil, fmt.Errorf("aiassist repo: %w", err)
	}
	walletStore, err := walletpg.NewRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("wallet store: %w", err)
	}
	walletSvc, err := walletusecase.NewService(walletStore, time.Now)
	if err != nil {
		return nil, fmt.Errorf("wallet usecase: %w", err)
	}
	policyStore, err := aipolicypg.New(pool)
	if err != nil {
		return nil, fmt.Errorf("aipolicy store: %w", err)
	}
	innerResolver, err := aipolicy.NewResolver(policyStore)
	if err != nil {
		return nil, fmt.Errorf("aipolicy resolver: %w", err)
	}
	policyResolver, err := aiassistadapter.NewPolicyResolver(innerResolver)
	if err != nil {
		return nil, fmt.Errorf("aipolicy bridge: %w", err)
	}
	consentStore, err := aipolicypg.NewConsentStore(pool)
	if err != nil {
		return nil, fmt.Errorf("consent store: %w", err)
	}
	consentSvc, err := aipolicy.NewConsentService(consentStore)
	if err != nil {
		return nil, fmt.Errorf("consent service: %w", err)
	}

	summarizer, err := assembleAIAssistSummarizer(aiAssistSummarizerDeps{
		Repo:              repo,
		Wallet:            walletSvc,
		Policy:            policyResolver,
		LLM:               llm,
		Consent:           consentSvc,
		Anonymizer:        regexanon.New(),
		AnonymizerVersion: regexanon.AnonymizerVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("aiassist service: %w", err)
	}
	log.Printf("crm: ai-assist operator summarizer wired (provider=openrouter, model=%s)", ReadAIAssistModel(getenv))
	return summarizer, nil
}
