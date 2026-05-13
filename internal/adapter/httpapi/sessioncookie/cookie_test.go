package sessioncookie

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetters_FlagsMatchADR0073 is acceptance criterion #5: every
// __Host-* cookie MUST carry Secure, the right SameSite, and the right
// HttpOnly. We assert by reading the Set-Cookie header back since
// http.Cookie's String() is the only direct view of all flags.
func TestSetters_FlagsMatchADR0073(t *testing.T) {
	cases := []struct {
		name              string
		set               func(http.ResponseWriter)
		wantPrefix        string
		wantSecure        bool
		wantHttpOnly      bool
		wantSameSite      string
		wantSessionMaxAge int
	}{
		{
			name:              "master",
			set:               func(w http.ResponseWriter) { SetMaster(w, "session-id-master", 4*60*60) },
			wantPrefix:        NameMaster + "=session-id-master",
			wantSecure:        true,
			wantHttpOnly:      true,
			wantSameSite:      "Strict",
			wantSessionMaxAge: 4 * 60 * 60,
		},
		{
			name:              "tenant",
			set:               func(w http.ResponseWriter) { SetTenant(w, "session-id-tenant", 8*60*60) },
			wantPrefix:        NameTenant + "=session-id-tenant",
			wantSecure:        true,
			wantHttpOnly:      true,
			wantSameSite:      "Lax",
			wantSessionMaxAge: 8 * 60 * 60,
		},
		{
			name:              "csrf",
			set:               func(w http.ResponseWriter) { SetCSRF(w, "csrf-token-value", 4*60*60) },
			wantPrefix:        NameCSRF + "=csrf-token-value",
			wantSecure:        true,
			wantHttpOnly:      false,
			wantSameSite:      "Strict",
			wantSessionMaxAge: 4 * 60 * 60,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.set(rec)
			h := rec.Header().Get("Set-Cookie")
			if !strings.HasPrefix(h, tc.wantPrefix) {
				t.Fatalf("Set-Cookie = %q, want prefix %q", h, tc.wantPrefix)
			}
			if !strings.Contains(h, "Path=/") {
				t.Fatalf("Set-Cookie missing Path=/: %q", h)
			}
			if tc.wantSecure && !strings.Contains(h, "Secure") {
				t.Fatalf("Set-Cookie missing Secure: %q", h)
			}
			if tc.wantHttpOnly && !strings.Contains(h, "HttpOnly") {
				t.Fatalf("Set-Cookie missing HttpOnly: %q", h)
			}
			if !tc.wantHttpOnly && strings.Contains(h, "HttpOnly") {
				t.Fatalf("Set-Cookie should NOT have HttpOnly (CSRF must be JS-readable): %q", h)
			}
			if !strings.Contains(h, "SameSite="+tc.wantSameSite) {
				t.Fatalf("Set-Cookie missing SameSite=%s: %q", tc.wantSameSite, h)
			}
			if !strings.Contains(h, "Max-Age=") {
				t.Fatalf("Set-Cookie missing Max-Age: %q", h)
			}
		})
	}
}

// TestSetters_HostPrefix asserts the __Host- prefix on every cookie name.
// Without it, the browser does not enforce Secure/Path=/ and the attacker
// can set the cookie from a sibling subdomain.
func TestSetters_HostPrefix(t *testing.T) {
	for _, name := range []string{NameMaster, NameTenant, NameCSRF} {
		if !strings.HasPrefix(name, "__Host-") {
			t.Fatalf("cookie name %q lacks __Host- prefix; ADR 0073 D2 violated", name)
		}
	}
}

func TestClearers_NegativeMaxAge(t *testing.T) {
	cases := []struct {
		name string
		fn   func(http.ResponseWriter)
		want string
	}{
		{"clear-master", ClearMaster, NameMaster},
		{"clear-tenant", ClearTenant, NameTenant},
		{"clear-csrf", ClearCSRF, NameCSRF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.fn(rec)
			h := rec.Header().Get("Set-Cookie")
			if !strings.HasPrefix(h, tc.want+"=") {
				t.Fatalf("Set-Cookie = %q, want prefix %q", h, tc.want+"=")
			}
			if !strings.Contains(h, "Max-Age=0") {
				t.Fatalf("clear cookie should set Max-Age=0 (negative MaxAge); got %q", h)
			}
		})
	}
}

func TestRead_PresentValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: NameTenant, Value: "abc123"})

	got, err := Read(r, NameTenant)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("got %q want %q", got, "abc123")
	}
}

func TestRead_AbsentReturnsErrCookieMissing(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := Read(r, NameTenant)
	if !errors.Is(err, ErrCookieMissing) {
		t.Fatalf("err = %v, want ErrCookieMissing", err)
	}
}

func TestRead_EmptyValueIsMissing(t *testing.T) {
	// A present-but-empty cookie is the artifact of a Clear call; treat
	// it as missing so a stale clear isn't mistaken for a live session.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Cookie", NameTenant+"=") // raw header — http.Cookie won't accept Value:""
	_, err := Read(r, NameTenant)
	// Stdlib drops empty-value cookies during parsing on some Go versions;
	// either way the contract is "no value, no read".
	if err == nil {
		t.Fatalf("Read of empty-value cookie should fail, got nil")
	}
}
