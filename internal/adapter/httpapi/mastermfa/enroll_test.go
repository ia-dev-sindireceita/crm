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

// fakeEnroller returns a scripted EnrollResult and tracks call counts.
type fakeEnroller struct {
	calls   int
	lastUID uuid.UUID
	lastLbl string
	res     mfa.EnrollResult
	err     error
}

func (f *fakeEnroller) Enroll(_ context.Context, uid uuid.UUID, label string) (mfa.EnrollResult, error) {
	f.calls++
	f.lastUID = uid
	f.lastLbl = label
	if f.err != nil {
		return mfa.EnrollResult{}, f.err
	}
	return f.res, nil
}

func sampleResult() mfa.EnrollResult {
	codes := make([]string, mfa.RecoveryCodeCount)
	for i := range codes {
		codes[i] = "ABCDE234567XYZ"[:mfa.RecoveryCodeLen] // 10 chars, RFC 4648
	}
	return mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita:ops%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Sindireceita&algorithm=SHA1&digits=6&period=30",
		SecretEncoded: "JBSWY3DPEHPK3PXP",
		RecoveryCodes: codes,
	}
}

func newReq(method string) *http.Request {
	r := httptest.NewRequest(method, "/m/2fa/enroll", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestEnrollHandler_PanicsOnNilEnroller(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewEnrollHandler(nil) did not panic")
		}
	}()
	mastermfa.NewEnrollHandler(nil, nil)
}

func TestEnrollHandler_RejectsNonPost(t *testing.T) {
	h := mastermfa.NewEnrollHandler(&fakeEnroller{res: sampleResult()}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newReq(method))
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status: got %d want 405", w.Code)
			}
			if got := w.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow: got %q want POST", got)
			}
		})
	}
}

func TestEnrollHandler_Returns401WhenNoMaster(t *testing.T) {
	h := mastermfa.NewEnrollHandler(&fakeEnroller{res: sampleResult()}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(http.MethodPost))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}

func TestEnrollHandler_HappyPath(t *testing.T) {
	enroller := &fakeEnroller{res: sampleResult()}
	h := mastermfa.NewEnrollHandler(enroller, nil)
	uid := uuid.New()
	r := newReq(http.MethodPost).WithContext(
		mastermfa.WithMaster(context.Background(), mastermfa.Master{ID: uid, Email: "ops@example.com"}),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	t.Run("status 200", func(t *testing.T) {
		if w.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200", w.Code)
		}
	})

	t.Run("Cache-Control prevents storage", func(t *testing.T) {
		cc := w.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control: got %q, expected no-store", cc)
		}
		if w.Header().Get("Pragma") != "no-cache" {
			t.Errorf("Pragma: got %q want no-cache", w.Header().Get("Pragma"))
		}
	})

	t.Run("Content-Type is HTML", func(t *testing.T) {
		ct := w.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type: got %q want text/html", ct)
		}
	})

	body, _ := io.ReadAll(w.Body)
	bodyS := string(body)

	t.Run("enroller called with master id + email", func(t *testing.T) {
		if enroller.calls != 1 {
			t.Errorf("Enroll calls: got %d want 1", enroller.calls)
		}
		if enroller.lastUID != uid {
			t.Errorf("userID: got %v want %v", enroller.lastUID, uid)
		}
		if enroller.lastLbl != "ops@example.com" {
			t.Errorf("label: got %q want ops@example.com", enroller.lastLbl)
		}
	})

	t.Run("response embeds otpauth URI + secret", func(t *testing.T) {
		if !strings.Contains(bodyS, "otpauth://totp/") {
			t.Error("body missing otpauth URI")
		}
		if !strings.Contains(bodyS, "JBSWY3DPEHPK3PXP") {
			t.Error("body missing base32 secret")
		}
	})

	t.Run("response embeds all 10 recovery codes (AC #2)", func(t *testing.T) {
		// Each formatted code appears twice: once in the visible <li>
		// and once in the copy-all button's data-codes attribute. With
		// 10 identical codes that is 20 substring matches in the body.
		// AC #2 ("only once in the response") is about the *response
		// instance*, not literal substring uniqueness — the codes are
		// in the document exactly once on this single response, which
		// the SecondPostAfterReloadDoesNotEchoOldCodes test confirms.
		formatted := mfa.FormatRecoveryCode("ABCDE23456")
		count := strings.Count(bodyS, formatted)
		if count != mfa.RecoveryCodeCount*2 {
			t.Errorf("formatted-code occurrences: got %d want %d (visible list + copy-all data)", count, mfa.RecoveryCodeCount*2)
		}
	})

	t.Run("response embeds dashed codes in visible <li> list", func(t *testing.T) {
		// Spot-check the visible list specifically. Locate the <ul
		// id="recovery-codes"> block and assert all 10 list entries
		// are inside it.
		const open = `id="recovery-codes"`
		idx := strings.Index(bodyS, open)
		if idx < 0 {
			t.Fatal("body missing <ul id=\"recovery-codes\">")
		}
		end := strings.Index(bodyS[idx:], "</ul>")
		if end < 0 {
			t.Fatal("body missing closing </ul> for recovery-codes list")
		}
		block := bodyS[idx : idx+end]
		formatted := mfa.FormatRecoveryCode("ABCDE23456")
		if got := strings.Count(block, "<li>"+formatted+"</li>"); got != mfa.RecoveryCodeCount {
			t.Errorf("visible-list <li> count: got %d want %d", got, mfa.RecoveryCodeCount)
		}
	})

	t.Run("offline-save warning is rendered (AC §1 wording)", func(t *testing.T) {
		// The template carries the explicit warning text; we assert on
		// a substring rather than the exact phrase so cosmetic edits
		// don't break the test.
		if !strings.Contains(bodyS, "uso único") {
			t.Error("body missing 'uso único' offline warning")
		}
		if !strings.Contains(bodyS, "não voltarão a aparecer") {
			t.Error("body missing 'will not appear again' confirmation warning")
		}
	})

	t.Run("copy-all button is present with codes payload", func(t *testing.T) {
		if !strings.Contains(bodyS, "Copiar todos os códigos") {
			t.Error("body missing copy-all button label")
		}
		if !strings.Contains(bodyS, "data-codes=") {
			t.Error("body missing data-codes attribute on copy button")
		}
	})

	t.Run("response is anti-indexable (robots meta)", func(t *testing.T) {
		// Belt-and-braces alongside Cache-Control. The HTML meta is
		// what defends against client-side caches that ignore the
		// header (e.g. 'View Page Source' written to disk by a browser
		// extension).
		if !strings.Contains(bodyS, `name="robots"`) {
			t.Error("body missing robots noindex meta")
		}
	})
}

func TestEnrollHandler_SecondPostAfterReloadDoesNotEchoOldCodes(t *testing.T) {
	// AC #2 says recovery codes appear "only once in the response".
	// The single-response semantics are encoded by Service.Enroll
	// minting fresh codes per call AND the handler embedding ONLY the
	// current call's response. We assert this by simulating two POSTs
	// with two distinct fake EnrollResults: the second response must
	// embed only the second set, never echo the first.
	enroller := &fakeEnroller{res: sampleResult()}
	h := mastermfa.NewEnrollHandler(enroller, nil)
	uid := uuid.New()
	mk := func() *http.Request {
		return newReq(http.MethodPost).WithContext(
			mastermfa.WithMaster(context.Background(), mastermfa.Master{ID: uid, Email: "ops@example.com"}),
		)
	}

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, mk())
	body1, _ := io.ReadAll(w1.Body)

	// Swap result before the second call.
	second := mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita:ops%40example.com?secret=NEWSECRET&issuer=Sindireceita",
		SecretEncoded: "NEWSECRET",
		RecoveryCodes: make([]string, mfa.RecoveryCodeCount),
	}
	for i := range second.RecoveryCodes {
		second.RecoveryCodes[i] = "ZZZZZ55555"[:mfa.RecoveryCodeLen]
	}
	enroller.res = second

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, mk())
	body2, _ := io.ReadAll(w2.Body)

	// Body 2 must contain the new secret, NOT the old one.
	if strings.Contains(string(body2), "JBSWY3DPEHPK3PXP") {
		t.Error("body2 leaked first-enrol secret")
	}
	if !strings.Contains(string(body2), "NEWSECRET") {
		t.Error("body2 missing second-enrol secret")
	}
	// And the first-enrol codes must not appear in body2.
	if strings.Contains(string(body2), mfa.FormatRecoveryCode("ABCDE23456")) {
		t.Error("body2 leaked first-enrol recovery codes")
	}
	// Confirm body1 had them (sanity).
	if !strings.Contains(string(body1), mfa.FormatRecoveryCode("ABCDE23456")) {
		t.Fatal("body1 missing first-enrol recovery codes (test self-check failed)")
	}
}

func TestEnrollHandler_MapsServiceErrorTo500WithGenericBody(t *testing.T) {
	enroller := &fakeEnroller{err: errors.New("internal boom")}
	h := mastermfa.NewEnrollHandler(enroller, nil)
	r := newReq(http.MethodPost).WithContext(
		mastermfa.WithMaster(context.Background(), mastermfa.Master{ID: uuid.New(), Email: "ops"}),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "internal boom") {
		t.Errorf("body leaked internal error message: %q", w.Body.String())
	}
}

func TestMasterFromContext(t *testing.T) {
	t.Run("missing returns ok=false", func(t *testing.T) {
		_, ok := mastermfa.MasterFromContext(context.Background())
		if ok {
			t.Fatal("ok=true on empty context")
		}
	})
	t.Run("nil ID returns ok=false", func(t *testing.T) {
		ctx := mastermfa.WithMaster(context.Background(), mastermfa.Master{ID: uuid.Nil})
		_, ok := mastermfa.MasterFromContext(ctx)
		if ok {
			t.Fatal("ok=true on nil ID")
		}
	})
	t.Run("populated returns ok=true", func(t *testing.T) {
		want := mastermfa.Master{ID: uuid.New(), Email: "ops@example.com"}
		ctx := mastermfa.WithMaster(context.Background(), want)
		got, ok := mastermfa.MasterFromContext(ctx)
		if !ok {
			t.Fatal("ok=false on populated context")
		}
		if got != want {
			t.Errorf("master: got %+v want %+v", got, want)
		}
	})
}
