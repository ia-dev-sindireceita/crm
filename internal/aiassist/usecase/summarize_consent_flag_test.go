package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/aiassist"
)

// TestSummarize_ConsentFlag_GatesOnConsentRequired exercises SIN-65363:
// the LGPD consent gate is opt-in by configuration. The
// aiassist.Policy.ConsentRequired flag is the primary gate; the
// deps-wired and PromptVersion!="" guards stay as defence in depth.
//
// The three rows map 1:1 onto the issue's acceptance criteria:
//
//	(a) ConsentRequired=false                  -> first click passes
//	    straight through; no ConsentRequired, no spend, consent not
//	    consulted.
//	(b) ConsentRequired=true + deps wired +
//	    PromptVersion!="" + no prior consent     -> *aiassist.ConsentRequired,
//	    nothing charged or fetched.
//	(c) ConsentRequired=true but deps NOT wired -> no-op (defence in
//	    depth preserved); the call proceeds to dispatch.
//
// All three are pure unit tests — no database, in-memory fakes only.
func TestSummarize_ConsentFlag_GatesOnConsentRequired(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		consentRequired bool
		gateWired       bool
		wantConsentErr  bool
		wantConsentCall int
		wantReserve     int
		wantLLM         int
	}{
		{
			name:            "off by default passes through",
			consentRequired: false,
			gateWired:       true,
			wantConsentErr:  false,
			wantConsentCall: 0,
			wantReserve:     1,
			wantLLM:         1,
		},
		{
			name:            "required and wired blocks first call",
			consentRequired: true,
			gateWired:       true,
			wantConsentErr:  true,
			wantConsentCall: 1,
			wantReserve:     0,
			wantLLM:         0,
		},
		{
			name:            "required but deps unwired is a no-op",
			consentRequired: true,
			gateWired:       false,
			wantConsentErr:  false,
			wantConsentCall: 0,
			wantReserve:     1,
			wantLLM:         1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newFixedClock(time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
			repo := newFakeRepo()
			w := newFakeWallet(1_000_000)
			llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 1, TokensOut: 1}}

			pol := defaultGatePolicy()
			pol.ConsentRequired = tc.consentRequired
			policy := &fakePolicy{policy: pol}

			consent := newFakeConsentService()
			anonymizer := &fakeAnonymizer{}

			var svc = newServiceForTest(t, repo, w, llm, policy, clock)
			if tc.gateWired {
				svc = newGatedService(t, repo, w, llm, policy, consent, anonymizer, clock)
			}

			_, err := svc.Summarize(context.Background(), defaultRequest())

			gotConsentErr := errors.Is(err, aiassist.ErrConsentRequired)
			if gotConsentErr != tc.wantConsentErr {
				t.Fatalf("ConsentRequired err = %v (err=%v); want %v", gotConsentErr, err, tc.wantConsentErr)
			}
			if !tc.wantConsentErr && err != nil {
				t.Fatalf("Summarize: unexpected error: %v", err)
			}
			if consent.hasCalls != tc.wantConsentCall {
				t.Errorf("consent.hasCalls = %d; want %d", consent.hasCalls, tc.wantConsentCall)
			}
			if w.reserveCalls != tc.wantReserve {
				t.Errorf("wallet.reserveCalls = %d; want %d", w.reserveCalls, tc.wantReserve)
			}
			if llm.callCount() != tc.wantLLM {
				t.Errorf("llm.calls = %d; want %d", llm.callCount(), tc.wantLLM)
			}
		})
	}
}
