package mastermfa_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// fakeRegenerator returns a scripted slice + error.
type fakeRegenerator struct {
	calls   int
	lastUID uuid.UUID
	codes   []string
	err     error
}

func (f *fakeRegenerator) RegenerateRecovery(_ context.Context, uid uuid.UUID) ([]string, error) {
	f.calls++
	f.lastUID = uid
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, len(f.codes))
	copy(out, f.codes)
	return out, nil
}

func sampleRegenCodes() []string {
	out := make([]string, mfa.RecoveryCodeCount)
	for i := range out {
		out[i] = "ZZZZZ77777"[:mfa.RecoveryCodeLen]
	}
	return out
}

func TestRegenerateHandler_PanicsOnNilDeps(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{Regenerator: nil})
}

func TestRegenerateHandler_RejectsNonPost(t *testing.T) {
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{
		Regenerator: &fakeRegenerator{codes: sampleRegenCodes()},
	})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/m/2fa/recovery/regenerate", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status: got %d want 405", w.Code)
			}
			if got := w.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow: got %q want POST", got)
			}
		})
	}
}

func TestRegenerateHandler_NoMasterReturns401(t *testing.T) {
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{
		Regenerator: &fakeRegenerator{codes: sampleRegenCodes()},
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/recovery/regenerate", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}

func TestRegenerateHandler_HappyPath(t *testing.T) {
	regen := &fakeRegenerator{codes: sampleRegenCodes()}
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{Regenerator: regen})
	uid := uuid.New()
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/recovery/regenerate", nil)
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if regen.calls != 1 {
		t.Errorf("Regenerator calls: got %d want 1", regen.calls)
	}
	if regen.lastUID != uid {
		t.Errorf("Regenerator userID: got %v want %v", regen.lastUID, uid)
	}

	body, _ := io.ReadAll(w.Body)
	bodyS := string(body)

	t.Run("Cache-Control prevents storage", func(t *testing.T) {
		if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Errorf("Cache-Control: got %q", w.Header().Get("Cache-Control"))
		}
	})

	t.Run("Body embeds 10 dash-formatted codes", func(t *testing.T) {
		formatted := mfa.FormatRecoveryCode("ZZZZZ77777")
		// Codes appear in visible <li> AND in data-codes attribute → 2x.
		if got := strings.Count(bodyS, formatted); got != mfa.RecoveryCodeCount*2 {
			t.Errorf("formatted-code occurrences: got %d want %d", got, mfa.RecoveryCodeCount*2)
		}
	})

	t.Run("Body has won't-appear-again warning", func(t *testing.T) {
		if !strings.Contains(bodyS, "não voltarão a aparecer") {
			t.Error("body missing 'won't appear again' warning")
		}
	})

	t.Run("Body has copy-all button", func(t *testing.T) {
		if !strings.Contains(bodyS, "data-codes=") {
			t.Error("body missing data-codes attribute on copy button")
		}
	})

	t.Run("Body has robots noindex meta", func(t *testing.T) {
		if !strings.Contains(bodyS, `name="robots"`) {
			t.Error("body missing robots meta")
		}
	})
}

func TestRegenerateHandler_PreviousCodesNotEchoedOnSecondRequest(t *testing.T) {
	// AC #3 strict: a second call MUST NOT echo the prior set.
	first := sampleRegenCodes()
	regen := &fakeRegenerator{codes: first}
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{Regenerator: regen})
	mk := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/m/2fa/recovery/regenerate", nil)
		return r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	}

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, mk())
	body1, _ := io.ReadAll(w1.Body)

	// Swap in a different code set.
	second := make([]string, mfa.RecoveryCodeCount)
	for i := range second {
		second[i] = "WWWWW55555"[:mfa.RecoveryCodeLen]
	}
	regen.codes = second

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, mk())
	body2, _ := io.ReadAll(w2.Body)

	if strings.Contains(string(body2), mfa.FormatRecoveryCode("ZZZZZ77777")) {
		t.Error("body2 leaked first regen's codes")
	}
	if !strings.Contains(string(body2), mfa.FormatRecoveryCode("WWWWW55555")) {
		t.Error("body2 missing second regen's codes")
	}
	// Sanity: body1 had the first set.
	if !strings.Contains(string(body1), mfa.FormatRecoveryCode("ZZZZZ77777")) {
		t.Fatal("body1 missing first regen's codes (test self-check failed)")
	}
}

func TestRegenerateHandler_ServiceErrorReturns500WithGenericBody(t *testing.T) {
	regen := &fakeRegenerator{err: errors.New("internal boom")}
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{Regenerator: regen})
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/recovery/regenerate", nil)
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "internal boom") {
		t.Errorf("body leaked internal error: %q", w.Body.String())
	}
}
