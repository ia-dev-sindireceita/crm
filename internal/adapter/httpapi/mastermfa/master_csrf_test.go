package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// reachedHandler records whether the wrapped handler ran and writes 200.
type reachedHandler struct{ reached bool }

func (h *reachedHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.reached = true
	w.WriteHeader(http.StatusOK)
}

func newMasterCSRF(host string, onReject func(*http.Request, mastermfa.OriginCSRFReason)) (func(http.Handler) http.Handler, *reachedHandler, http.Handler) {
	inner := &reachedHandler{}
	mw := mastermfa.RequireMasterOriginCSRF(mastermfa.RequireMasterOriginCSRFConfig{
		MasterHost: host,
		OnReject:   onReject,
	})
	return mw, inner, mw(inner)
}

func TestRequireMasterOriginCSRF_SafeMethodsPassThrough(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		_, inner, h := newMasterCSRF(testMasterHost, nil)
		// Deliberately hostile Origin: a safe method must NOT be gated.
		r := httptest.NewRequest(m, "/master/tenants", nil)
		r.Header.Set("Origin", "https://acme.crm.local")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if !inner.reached {
			t.Fatalf("%s: safe method should pass through, handler not reached", m)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", m, rec.Code)
		}
	}
}

func TestRequireMasterOriginCSRF_OffOriginPOSTRejected(t *testing.T) {
	var gotReason mastermfa.OriginCSRFReason
	_, inner, h := newMasterCSRF(testMasterHost, func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		gotReason = reason
	})
	// CSRF-7 #1: valid-session shape, forged cross-tenant Origin.
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	r.Header.Set("Origin", "https://acme.crm.local")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if inner.reached {
		t.Fatal("off-origin POST must NOT reach the handler (no mutation)")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if gotReason != mastermfa.OriginCSRFReasonMismatch {
		t.Fatalf("want mismatch reason, got %q", gotReason)
	}
}

func TestRequireMasterOriginCSRF_BothHeadersAbsentRejected(t *testing.T) {
	var gotReason mastermfa.OriginCSRFReason
	_, inner, h := newMasterCSRF(testMasterHost, func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		gotReason = reason
	})
	// CSRF-7 #2: fail closed when both Origin and Referer are absent.
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if inner.reached {
		t.Fatal("POST with no Origin/Referer must fail closed")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if gotReason != mastermfa.OriginCSRFReasonMissing {
		t.Fatalf("want missing reason, got %q", gotReason)
	}
}

func TestRequireMasterOriginCSRF_OnOriginPOSTReachesHandler(t *testing.T) {
	_, inner, h := newMasterCSRF(testMasterHost, nil)
	// CSRF-7 #3: positive twin — Origin == canonical master origin.
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	r.Header.Set("Origin", "https://"+testMasterHost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !inner.reached {
		t.Fatal("on-origin POST should reach the handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

func TestRequireMasterOriginCSRF_RefererFallback(t *testing.T) {
	tests := []struct {
		name        string
		referer     string
		wantReached bool
		wantReason  mastermfa.OriginCSRFReason
	}{
		{
			name:        "valid referer when origin absent reaches handler",
			referer:     "https://" + testMasterHost + "/master/tenants",
			wantReached: true,
		},
		{
			name:       "mismatched referer rejected",
			referer:    "https://acme.crm.local/funnel",
			wantReason: mastermfa.OriginCSRFReasonMismatch,
		},
		{
			name:       "suffix-attack referer rejected (no substring match)",
			referer:    "https://master.crm.local.evil.com/master/tenants",
			wantReason: mastermfa.OriginCSRFReasonMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotReason mastermfa.OriginCSRFReason
			_, inner, h := newMasterCSRF(testMasterHost, func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
				gotReason = reason
			})
			r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
			r.Header.Set("Referer", tc.referer)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if inner.reached != tc.wantReached {
				t.Fatalf("reached = %v, want %v", inner.reached, tc.wantReached)
			}
			if tc.wantReached {
				if rec.Code != http.StatusOK {
					t.Fatalf("want 200, got %d", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d", rec.Code)
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestRequireMasterOriginCSRF_OriginPreferredOverReferer(t *testing.T) {
	// When Origin is present it is authoritative; a benign Referer must
	// NOT rescue a hostile Origin (C4: Referer is consulted only when
	// Origin is absent).
	var gotReason mastermfa.OriginCSRFReason
	_, inner, h := newMasterCSRF(testMasterHost, func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		gotReason = reason
	})
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	r.Header.Set("Origin", "https://acme.crm.local")
	r.Header.Set("Referer", "https://"+testMasterHost+"/master/tenants")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if inner.reached {
		t.Fatal("hostile Origin must not be rescued by a benign Referer")
	}
	if gotReason != mastermfa.OriginCSRFReasonMismatch {
		t.Fatalf("want mismatch, got %q", gotReason)
	}
}

func TestRequireMasterOriginCSRF_SchemeAndParseGuards(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		wantReason mastermfa.OriginCSRFReason
	}{
		{"http scheme rejected", "http://" + testMasterHost, mastermfa.OriginCSRFReasonSchemeNotHTTPS},
		{"null origin falls through to missing", "null", mastermfa.OriginCSRFReasonMissing},
		{"unparseable origin rejected", "://bad", mastermfa.OriginCSRFReasonUnparsable},
		{"origin with port still matches host", "https://" + testMasterHost + ":8443", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotReason mastermfa.OriginCSRFReason
			_, inner, h := newMasterCSRF(testMasterHost, func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
				gotReason = reason
			})
			r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
			r.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if tc.wantReason == "" {
				if !inner.reached {
					t.Fatalf("origin %q should reach handler", tc.origin)
				}
				return
			}
			if inner.reached {
				t.Fatalf("origin %q should be rejected", tc.origin)
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestRequireMasterOriginCSRF_EmptyMasterHostFailsClosed(t *testing.T) {
	// CSRF-3: unset MasterHost must reject every unsafe request, even an
	// otherwise-canonical-looking Origin — never match-any.
	var gotReason mastermfa.OriginCSRFReason
	_, inner, h := newMasterCSRF("", func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		gotReason = reason
	})
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	r.Header.Set("Origin", "https://"+testMasterHost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if inner.reached {
		t.Fatal("empty MasterHost must fail closed")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if gotReason != mastermfa.OriginCSRFReasonHostUnset {
		t.Fatalf("want host_unset, got %q", gotReason)
	}
}
