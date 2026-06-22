package usermfa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// TestVerifyGETRendersPithoCard locks in the SIN-65102 / Tranche C visual
// port of the 2FA challenge screen: it must render inside the shared Pitho
// login card (tokens + components + login.css), carry the Pitho wordmark,
// and use the .btn--primary primitive — matching the /login surface so the
// password and TOTP steps look like one flow. The behavior-carrying form
// field (name="code") and the CSRF/next hidden inputs must survive the port.
func TestVerifyGETRendersPithoCard(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/inbox"})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	body := w.Body.String()

	wants := []string{
		`class="login-card"`,                    // shared Pitho card shell
		`data-testid="login-wordmark"`,          // Pitho wordmark, no emoji
		`/static/css/login.css`,                 // token-driven stylesheet linked
		`/static/css/tokens.css`,                // design tokens linked
		`class="btn btn--primary`,               // submit uses the button primitive
		`name="code"`,                           // behavior-carrying field preserved
		`name="csrf"`,                           // CSRF hidden input preserved
		`name="next"`,                           // post-verify redirect preserved
		`autocomplete="one-time-code"`,          // TOTP affordance preserved
		`pitho-logo--light`, `pitho-logo--dark`, // both logo variants for light+dark
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("verify card missing %q\nbody:\n%s", want, body)
		}
	}
}
