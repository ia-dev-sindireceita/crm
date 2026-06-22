package usermfa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// externalQRHosts are services that, if referenced from the page, would
// exfiltrate the TOTP secret embedded in the otpauth URI to a third party.
// The QR MUST be generated server-side and inlined; none of these may
// appear in the rendered enrollment page or in the SVG helper output.
var externalQRHosts = []string{
	"qrserver.com",
	"chart.googleapis.com",
	"chart.apis.google",
	"quickchart.io",
	"goqr.me",
}

// TestOTPAuthQRCodeSVG_IsInlineAndLeaksNoSecret asserts the helper emits a
// self-contained inline SVG and never points at an external QR host.
func TestOTPAuthQRCodeSVG_IsInlineAndLeaksNoSecret(t *testing.T) {
	t.Parallel()
	const uri = "otpauth://totp/Sindireceita:agent@acme.test?secret=ABCDEFGHJKLMNP&issuer=Pitho"
	svg, err := otpauthQRCodeSVG(uri)
	if err != nil {
		t.Fatalf("otpauthQRCodeSVG: %v", err)
	}
	got := string(svg)
	if got == "" {
		t.Fatal("expected non-empty SVG")
	}
	for _, want := range []string{"<svg", "role=\"img\"", "</svg>", "<rect"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SVG missing %q:\n%s", want, got)
		}
	}
	// Review point #1: no external QR host, and no <img>/href fetch at all.
	for _, host := range externalQRHosts {
		if strings.Contains(got, host) {
			t.Fatalf("SVG must be inline; found external reference %q:\n%s", host, got)
		}
	}
	if strings.Contains(got, "<img") || strings.Contains(got, "src=") || strings.Contains(got, "href=") {
		t.Fatalf("SVG must not fetch any resource, got:\n%s", got)
	}
	// The secret must NOT appear verbatim in the markup — it is encoded
	// into the QR module matrix, not interpolated into attributes.
	if strings.Contains(got, "ABCDEFGHJKLMNP") {
		t.Fatalf("secret leaked into SVG markup:\n%s", got)
	}
}

// TestOTPAuthQRCodeSVG_Deterministic guards against accidental nondeterminism
// (e.g. a future map-iteration change) so golden assertions stay stable.
func TestOTPAuthQRCodeSVG_Deterministic(t *testing.T) {
	t.Parallel()
	const uri = "otpauth://totp/Sindireceita:agent@acme.test?secret=ABC"
	a, err := otpauthQRCodeSVG(uri)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := otpauthQRCodeSVG(uri)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a != b {
		t.Fatal("otpauthQRCodeSVG is not deterministic for a fixed URI")
	}
}

// TestSetupRendersAppShellAndInlineQR is the AC verification: the enrollment
// page renders in the app-shell (tokens/components/login CSS + Pitho
// wordmark), shows a scannable inline SVG QR, keeps the base32 + otpauth
// manual fallback and recovery codes, and references no external QR host.
func TestSetupRendersAppShellAndInlineQR(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.labels.set(user, "agent@acme.test")
	deps.enrollment.mark(user, false)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodGet, "/admin/2fa/setup", tenant, session, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	body := w.Body.String()

	// AC #1 — app-shell parity with already_enrolled.html / verify.html.
	for _, want := range []string{
		"/static/css/tokens.css",
		"/static/css/components.css",
		"/static/css/login.css",
		"/static/css/brand.css",
		"class=\"login-page\"",
		"login-card__wordmark",
		"alt=\"Pitho\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("app-shell: expected body to contain %q", want)
		}
	}

	// AC #2 — scannable inline SVG QR.
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "role=\"img\"") {
		t.Fatalf("expected an inline <svg> QR in the page")
	}
	// AC #2 — base32 secret + otpauth fallback and recovery codes preserved.
	for _, want := range []string{"ABCDEFGHJKLMNPQRSTUVWXYZ234567", "otpauth://totp", "AAAAA-AAAAA"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected fallback content %q in page", want)
		}
	}

	// Review point #1 — the QR asset must not reference an external host.
	for _, host := range externalQRHosts {
		if strings.Contains(body, host) {
			t.Fatalf("page references external QR host %q — TOTP secret exfiltration risk", host)
		}
	}

	// No regression in the silent-rotation guard accounting.
	if enroller.count() != 1 {
		t.Fatalf("Enroll calls: want 1 got %d", enroller.count())
	}
}
