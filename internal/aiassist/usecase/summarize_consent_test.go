package usecase_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// fakeConsentService is the in-memory test double for the consent
// gate. It deduplicates by (tenant, kind, id) and records every
// HasConsent / RecordConsent call so the integration test can assert
// that the gate (a) blocks on first call, (b) does not call wallet or
// LLM, and (c) passes through after RecordConsent.
type fakeConsentService struct {
	mu       sync.Mutex
	rows     map[string]consentRow
	err      error
	hasCalls int
}

type consentRow struct {
	hash      [32]byte
	anonVer   string
	promptVer string
}

func newFakeConsentService() *fakeConsentService {
	return &fakeConsentService{rows: map[string]consentRow{}}
}

func (f *fakeConsentService) keyFor(scope aipolicy.ConsentScope) string {
	return scope.TenantID.String() + "|" + string(scope.Kind) + "|" + scope.ID
}

func (f *fakeConsentService) HasConsent(_ context.Context, scope aipolicy.ConsentScope, anonVer, promptVer string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasCalls++
	if f.err != nil {
		return false, f.err
	}
	row, ok := f.rows[f.keyFor(scope)]
	if !ok {
		return false, nil
	}
	if row.anonVer != anonVer || row.promptVer != promptVer {
		return false, nil
	}
	return true, nil
}

// record is the test-side equivalent of aipolicy.ConsentService.RecordConsent;
// the gate itself doesn't call it (only the future web handler will).
func (f *fakeConsentService) record(scope aipolicy.ConsentScope, payloadPreview, anonVer, promptVer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[f.keyFor(scope)] = consentRow{
		hash:      sha256.Sum256([]byte(payloadPreview)),
		anonVer:   anonVer,
		promptVer: promptVer,
	}
}

// fakeAnonymizer turns any input into a deterministic anonymized
// preview by prefixing with "ANON:". Production uses the regex
// adapter; the gate only cares that the function is called.
type fakeAnonymizer struct {
	mu    sync.Mutex
	err   error
	calls int
	last  string
}

func (a *fakeAnonymizer) Anonymize(_ context.Context, in string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.last = in
	if a.err != nil {
		return "", a.err
	}
	return "ANON:" + in, nil
}

func defaultGatePolicy() aiassist.Policy {
	return aiassist.Policy{
		AIEnabled:       true,
		OptIn:           true,
		Anonymize:       true,
		Model:           "google/gemini-2.0-flash",
		MaxOutputTokens: 256,
		PromptVersion:   "prompt-v1",
	}
}

func newGatedService(
	t *testing.T,
	repo aiassist.SummaryRepository,
	w aiassist.WalletClient,
	llm aiassist.LLMClient,
	pol aiassist.PolicyResolver,
	consent aiassist.ConsentService,
	anonymizer aiassist.Anonymizer,
	clock *fixedClock,
) *usecase.Service {
	t.Helper()
	cfg := usecase.Config{
		Repo:              repo,
		Wallet:            w,
		LLM:               llm,
		Policy:            pol,
		Consent:           consent,
		Anonymizer:        anonymizer,
		AnonymizerVersion: "anon-v1",
		Clock:             clock.Now,
	}
	svc, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestSummarize_ConsentGate_FirstCallRequiresConsent(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	req := defaultRequest()
	_, err := svc.Summarize(context.Background(), req)
	if !errors.Is(err, aiassist.ErrConsentRequired) {
		t.Fatalf("err = %v; want wrap of ErrConsentRequired", err)
	}

	var cr *aiassist.ConsentRequired
	if !errors.As(err, &cr) {
		t.Fatalf("err = %v; want errors.As(*ConsentRequired)", err)
	}
	if cr.Scope.TenantID != req.TenantID {
		t.Errorf("Scope.TenantID = %v; want %v", cr.Scope.TenantID, req.TenantID)
	}
	if cr.Scope.Kind != aipolicy.ScopeTenant {
		t.Errorf("Scope.Kind = %q; want tenant", cr.Scope.Kind)
	}
	if cr.AnonymizerVersion != "anon-v1" {
		t.Errorf("AnonymizerVersion = %q; want anon-v1", cr.AnonymizerVersion)
	}
	if cr.PromptVersion != "prompt-v1" {
		t.Errorf("PromptVersion = %q; want prompt-v1", cr.PromptVersion)
	}
	if !strings.HasPrefix(cr.Payload, "ANON:") {
		t.Errorf("Payload not anonymized: %q", cr.Payload)
	}
	if cr.Payload == req.Prompt {
		t.Errorf("Payload equals raw prompt; gate must run anonymizer first")
	}

	// AC: wallet + LLM must NOT be touched on the consent-blocked
	// path; defence in depth that no spend or upstream traffic can
	// land before consent.
	if w.reserveCalls != 0 {
		t.Errorf("wallet.reserveCalls = %d; want 0 on consent-blocked call", w.reserveCalls)
	}
	if llm.callCount() != 0 {
		t.Errorf("llm.calls = %d; want 0 on consent-blocked call", llm.callCount())
	}
	if anonymizer.calls != 1 {
		t.Errorf("anonymizer.calls = %d; want 1", anonymizer.calls)
	}
	if consent.hasCalls != 1 {
		t.Errorf("consent.hasCalls = %d; want 1", consent.hasCalls)
	}
}

func TestSummarize_ConsentGate_SecondCallProceedsAfterRecord(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 7, TokensOut: 13}}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); !errors.Is(err, aiassist.ErrConsentRequired) {
		t.Fatalf("first call: err = %v; want ErrConsentRequired", err)
	}

	// Web handler equivalent: persist consent for the scope.
	scope := aipolicy.ConsentScope{
		TenantID: req.TenantID,
		Kind:     aipolicy.ScopeTenant,
		ID:       req.TenantID.String(),
	}
	consent.record(scope, "ANON:"+req.Prompt, "anon-v1", "prompt-v1")

	resp, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if resp == nil || resp.Summary == nil {
		t.Fatal("second call: nil response")
	}
	if w.reserveCalls != 1 {
		t.Errorf("wallet.reserveCalls = %d; want 1 after consent", w.reserveCalls)
	}
	if llm.callCount() != 1 {
		t.Errorf("llm.calls = %d; want 1 after consent", llm.callCount())
	}
}

func TestSummarize_ConsentGate_PromptVersionBumpForcesReConsent(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)
	req := defaultRequest()

	scope := aipolicy.ConsentScope{
		TenantID: req.TenantID,
		Kind:     aipolicy.ScopeTenant,
		ID:       req.TenantID.String(),
	}
	consent.record(scope, "ANON:"+req.Prompt, "anon-v1", "prompt-v0")

	// Policy has PromptVersion="prompt-v1"; consent stored is "prompt-v0";
	// gate must report not-consented.
	_, err := svc.Summarize(context.Background(), req)
	if !errors.Is(err, aiassist.ErrConsentRequired) {
		t.Fatalf("err = %v; want ErrConsentRequired after prompt version bump", err)
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Errorf("wallet/llm touched on bumped-version call: reserve=%d, llm=%d", w.reserveCalls, llm.callCount())
	}
}

func TestSummarize_ConsentGate_SkippedWhenPolicyVersionEmpty(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: aiassist.Policy{
		AIEnabled:       true,
		OptIn:           true,
		Model:           "m",
		MaxOutputTokens: 256,
		// PromptVersion left empty intentionally — gate is skipped.
	}}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if consent.hasCalls != 0 {
		t.Errorf("consent.hasCalls = %d; want 0 when policy has no PromptVersion", consent.hasCalls)
	}
	if anonymizer.calls != 0 {
		t.Errorf("anonymizer.calls = %d; want 0 when gate skipped", anonymizer.calls)
	}
}

func TestSummarize_ConsentGate_AnonymizerErrorAborts(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonErr := errors.New("anonymize boom")
	anonymizer := &fakeAnonymizer{err: anonErr}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, anonErr) {
		t.Fatalf("err = %v; want wrap of anonErr", err)
	}
	if consent.hasCalls != 0 {
		t.Errorf("consent must not be called after anonymizer failure")
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Errorf("wallet/llm touched after anonymizer failure")
	}
}

func TestSummarize_ConsentGate_ConsentErrorAborts(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	cErr := errors.New("consent service boom")
	consent.err = cErr
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, cErr) {
		t.Fatalf("err = %v; want wrap of consent error", err)
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Errorf("wallet/llm touched after consent failure")
	}
}

func TestSummarize_ConsentGate_ChannelScopeWinsOverTeam(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "x", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	req := defaultRequest()
	req.Scope = aiassist.Scope{TeamID: uuid.New().String(), ChannelID: "whatsapp"}

	_, err := svc.Summarize(context.Background(), req)
	var cr *aiassist.ConsentRequired
	if !errors.As(err, &cr) {
		t.Fatalf("err = %v; want ConsentRequired", err)
	}
	if cr.Scope.Kind != aipolicy.ScopeChannel {
		t.Errorf("Scope.Kind = %q; want channel (channel must win over team)", cr.Scope.Kind)
	}
	if cr.Scope.ID != "whatsapp" {
		t.Errorf("Scope.ID = %q; want whatsapp", cr.Scope.ID)
	}
}

func TestSummarize_ConsentGate_TeamScopeWhenChannelAbsent(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultGatePolicy()}
	consent := newFakeConsentService()
	anonymizer := &fakeAnonymizer{}
	svc := newGatedService(t, repo, w, llm, pol, consent, anonymizer, clock)

	teamID := uuid.New().String()
	req := defaultRequest()
	req.Scope = aiassist.Scope{TeamID: teamID}

	_, err := svc.Summarize(context.Background(), req)
	var cr *aiassist.ConsentRequired
	if !errors.As(err, &cr) {
		t.Fatalf("err = %v; want ConsentRequired", err)
	}
	if cr.Scope.Kind != aipolicy.ScopeTeam {
		t.Errorf("Scope.Kind = %q; want team", cr.Scope.Kind)
	}
	if cr.Scope.ID != teamID {
		t.Errorf("Scope.ID = %q; want %q", cr.Scope.ID, teamID)
	}
}

func TestNewService_RejectsPartialGateWiring(t *testing.T) {
	t.Parallel()
	base := usecase.Config{
		Repo:   newFakeRepo(),
		Wallet: newFakeWallet(1),
		LLM:    &fakeLLM{},
		Policy: &fakePolicy{policy: defaultPolicy()},
	}
	cases := []struct {
		name   string
		mutate func(*usecase.Config)
	}{
		{"only consent", func(c *usecase.Config) { c.Consent = newFakeConsentService() }},
		{"only anonymizer", func(c *usecase.Config) { c.Anonymizer = &fakeAnonymizer{} }},
		{"only anon version", func(c *usecase.Config) { c.AnonymizerVersion = "v1" }},
		{
			"consent + version, no anonymizer",
			func(c *usecase.Config) {
				c.Consent = newFakeConsentService()
				c.AnonymizerVersion = "v1"
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			tc.mutate(&cfg)
			if _, err := usecase.NewService(cfg); err == nil {
				t.Errorf("expected error for partial wiring (%s)", tc.name)
			}
		})
	}
}
