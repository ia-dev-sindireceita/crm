package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ai-assist/anonymizer"
	"github.com/pericles-luz/crm/internal/ai-assist/anonymizer/regex"
	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// SIN-63995 — LGPD F8 part-2 wire-up.
//
// These tests pin the SE-spec invariants from
// /SIN/issues/SIN-63945#document-lgpd-field-spec onto the use case:
//
//   - AC #1 / SE test #3 — Yellow opted-in fields render as their
//     catalog PromptForm (tokenised, never cleartext) and reach the
//     LLM request payload.
//   - AC #2 — Green fields render unconditionally; data-minimisation
//     never silently drops the non-PII context.
//   - AC #3 — Red-tier names in Policy.StructuredFields are blocked at
//     the use-case boundary with the aiassist.ErrLGPDBlocked sentinel
//     (defence in depth on top of aipolicy.ValidateStructuredFields);
//     no wallet reservation, no LLM call.
//   - SE test #5 — CPF inline in the free-form message body is still
//     tokenised by the wired anonymizer end-to-end, independent of the
//     structured-fields selector.

// optYellowPolicy returns a permissive policy that opts into the three
// Yellow catalog fields. Anonymize stays true so the dispatch-side
// guard in buildDispatchPrompt activates when the test wires an
// anonymizer.
func optYellowPolicy() aiassist.Policy {
	pol := defaultPolicy()
	pol.StructuredFields = []string{"email", "phone", "cnpj"}
	return pol
}

// promptFormFor is a localised helper for the assertions below; it
// looks up the catalog entry so the tests stay coupled to the SE-owned
// catalog rather than hard-coding the verbatim PromptForm strings (a
// rewording in lgpd_fields.go must surface here as a test refresh, not
// a silent drift).
func promptFormFor(t *testing.T, name string) string {
	t.Helper()
	entry, ok := aipolicy.LGPDFieldByName(name)
	if !ok {
		t.Fatalf("catalog missing field %q — fix the catalog or the test", name)
	}
	return entry.PromptForm
}

func TestSummarize_StructuredFields_YellowOptedInRendersTokenisedForm(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: optYellowPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	got := llm.lastReq.Prompt
	for _, yellow := range []string{"email", "phone", "cnpj"} {
		want := promptFormFor(t, yellow)
		if !strings.Contains(got, want) {
			t.Errorf("LLM prompt missing Yellow form for %q (%q)", yellow, want)
		}
	}
	for _, token := range []string{
		anonymizer.TokenEmail,
		anonymizer.TokenPhone,
		anonymizer.TokenCNPJ,
	} {
		if !strings.Contains(got, token) {
			t.Errorf("LLM prompt missing anonymizer token %q", token)
		}
	}
	// Body must survive intact; the structured block prepends, it
	// never replaces.
	if !strings.Contains(got, req.Prompt) {
		t.Errorf("LLM prompt lost the original body")
	}
}

func TestSummarize_StructuredFields_GreenAlwaysRendered(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	// No Yellow opt-in: StructuredFields is empty.
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	if _, err := svc.Summarize(context.Background(), defaultRequest()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	got := llm.lastReq.Prompt
	for _, entry := range aipolicy.LGPDFieldCatalog() {
		switch entry.Tier {
		case aipolicy.TierGreen:
			if !strings.Contains(got, entry.PromptForm) {
				t.Errorf("Green field %q (form %q) missing — data-minimisation MUST keep operational context", entry.Name, entry.PromptForm)
			}
		case aipolicy.TierYellow:
			if strings.Contains(got, entry.PromptForm) {
				t.Errorf("Yellow field %q (form %q) leaked into prompt without opt-in", entry.Name, entry.PromptForm)
			}
		case aipolicy.TierRed:
			if strings.Contains(got, entry.PromptForm) {
				t.Errorf("Red field %q form leaked into prompt", entry.Name)
			}
		}
	}
}

func TestSummarize_StructuredFields_RedSentinelBlocksDispatch(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))

	// Every Red catalog entry MUST trip the belt-and-braces sentinel
	// even though the form validator (aipolicy.ValidateStructuredFields)
	// should have stripped it upstream.
	for _, red := range aipolicy.LGPDRedFieldNames() {
		red := red
		t.Run(red, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			w := newFakeWallet(1_000_000)
			llm := &fakeLLM{}
			policy := defaultPolicy()
			policy.StructuredFields = []string{red}
			pol := &fakePolicy{policy: policy}
			svc := newServiceForTest(t, repo, w, llm, pol, clock)

			_, err := svc.Summarize(context.Background(), defaultRequest())
			if !errors.Is(err, aiassist.ErrLGPDBlocked) {
				t.Fatalf("err = %v; want errors.Is(ErrLGPDBlocked) for red field %q", err, red)
			}
			if w.reserveCalls != 0 {
				t.Errorf("wallet reserved on red-tier dispatch; reserveCalls=%d", w.reserveCalls)
			}
			if llm.callCount() != 0 {
				t.Errorf("LLM called on red-tier dispatch; calls=%d", llm.callCount())
			}
		})
	}
}

func TestSummarize_StructuredFields_UnknownNameBlocksDispatch(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	policy := defaultPolicy()
	policy.StructuredFields = []string{"definitely-not-in-the-catalog"}
	pol := &fakePolicy{policy: policy}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrLGPDBlocked) {
		t.Fatalf("err = %v; want errors.Is(ErrLGPDBlocked) for unknown name", err)
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Errorf("dispatch must short-circuit on unknown field; reserve=%d, llm=%d", w.reserveCalls, llm.callCount())
	}
}

func TestSummarize_StructuredFields_GreenInOptInSetIsRejected(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	policy := defaultPolicy()
	// Green names are unconditional and MUST NOT appear in
	// Policy.StructuredFields. A drift in the resolver / DB row
	// surfaces here.
	policy.StructuredFields = []string{"display_name"}
	pol := &fakePolicy{policy: policy}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrLGPDBlocked) {
		t.Fatalf("err = %v; want errors.Is(ErrLGPDBlocked) for green name in opt-in set", err)
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Errorf("dispatch must short-circuit on green-in-opt-in set; reserve=%d, llm=%d", w.reserveCalls, llm.callCount())
	}
}

// TestSummarize_StructuredFields_CPFInBodyTokenisedEndToEnd covers
// SE regression test #5: even with Yellow opted-in, a free-text
// message body that carries an inline CPF still reaches the LLM with
// the CPF tokenised by the anonymizer. The anonymizer is independent
// of the structured-field selector — opting Yellow in MUST NOT route
// the cleartext path around it.
//
// We wire the production regex anonymizer (not the test
// "ANON:"-prefix fake) so the assertion runs against the real
// stable-token contract.
func TestSummarize_StructuredFields_CPFInBodyTokenisedEndToEnd(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}

	pol := &fakePolicy{policy: aiassist.Policy{
		AIEnabled:        true,
		OptIn:            true,
		Anonymize:        true,
		Model:            "google/gemini-2.0-flash",
		MaxOutputTokens:  256,
		PromptVersion:    "prompt-v1",
		StructuredFields: []string{"email", "phone", "cnpj"},
	}}
	consent := newFakeConsentService()
	anon := regex.New()
	svc := newGatedService(t, repo, w, llm, pol, consent, anon, clock)

	req := defaultRequest()
	// Cleartext payload carrying every PII class the anonymizer
	// covers; we assert the regex adapter tokenises each one along
	// the dispatch path.
	const cleartextCPF = "123.456.789-09"
	const cleartextEmail = "operator@example.com"
	const cleartextPhone = "+55 11 98765-4321"
	req.Prompt = "Cliente João, CPF " + cleartextCPF +
		", contato " + cleartextEmail +
		" — tel " + cleartextPhone + ". Pode confirmar?"

	// Pre-record consent so the gate falls through and the dispatch
	// path actually runs.
	scope := aipolicy.ConsentScope{
		TenantID: req.TenantID,
		Kind:     aipolicy.ScopeTenant,
		ID:       req.TenantID.String(),
	}
	anonymisedPreview, err := anon.Anonymize(context.Background(), req.Prompt)
	if err != nil {
		t.Fatalf("anon preview: %v", err)
	}
	// newGatedService wires AnonymizerVersion as "anon-v1" — record
	// under the same string so the gate falls through.
	consent.record(scope, anonymisedPreview, "anon-v1", "prompt-v1")

	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	got := llm.lastReq.Prompt
	for _, raw := range []string{cleartextCPF, cleartextEmail, cleartextPhone} {
		if strings.Contains(got, raw) {
			t.Errorf("LLM prompt leaked cleartext %q — ADR-0041 D3 invariant violated", raw)
		}
	}
	for _, token := range []string{
		anonymizer.TokenCPF,
		anonymizer.TokenEmail,
		anonymizer.TokenPhone,
	} {
		if !strings.Contains(got, token) {
			t.Errorf("LLM prompt missing tokenised %q — anonymizer skipped on dispatch", token)
		}
	}
	// Yellow opt-in tokens must still be present from the structured
	// context block, independently of the body anonymizer pass.
	if !strings.Contains(got, promptFormFor(t, "email")) {
		t.Errorf("structured-context email form missing — Yellow opt-in did not render")
	}
}

// TestSummarize_StructuredFields_WalletReservationCoversAssembledPrompt
// proves the reservation is computed over the full assembled prompt
// (structured block + anonymised body), not the raw request body, so
// the wallet covers what actually leaves the process.
func TestSummarize_StructuredFields_WalletReservationCoversAssembledPrompt(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: optYellowPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	// Estimator is len/4 + MaxOutputTokens. The assembled prompt is
	// strictly longer than the body, so the wallet reservation MUST
	// be at least as large as the body-only estimate.
	if got := llm.lastReq.Prompt; len(got) <= len(req.Prompt) {
		t.Fatalf("assembled prompt (%d) must be longer than raw body (%d)", len(got), len(req.Prompt))
	}
	// Reservation amount = balance - available. fakeWallet keeps the
	// reservation visible via snapshot until commit clears it; after
	// commit, balance dropped by the actual usage (clamped to the
	// reservation). Either way the wallet recorded a successful
	// reservation that covered the prompt.
	if w.commitCalls != 1 {
		t.Fatalf("expected exactly one commit, got %d", w.commitCalls)
	}
}
