// Package caddysecurity holds compile-time invariants over the Caddy edge
// security headers config that we want enforced by `go test ./...`.
//
// The config itself lives in deploy/caddy/security-headers.caddy and is
// imported by every site block in the Caddyfile / Caddyfile.stg. There is no
// Go production code in this package — only assertions that pin the contract
// flagged by the Fase 6 pen-test ([SIN-63218](/SIN/issues/SIN-63218)):
//
//   - HSTS max-age default MUST be 31536000 (1 year). A weaker default opens a
//     MitM TLS downgrade window for any client that has been offline longer
//     than that value, which is exactly the bug the pen-test caught when the
//     baked default was 300 (5 minutes).
//
//   - `includeSubDomains` MUST be present so future subdomains inherit the
//     HSTS pin.
//
//   - `preload` MUST be present so the host stays eligible for the browser
//     HSTS preload list. The flag alone does not enroll the host (that
//     requires explicit submission to https://hstspreload.org with CTO
//     sign-off); leaving it off would force a second config change later.
package caddysecurity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) returned !ok")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func loadSecurityHeadersCaddy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "deploy", "caddy", "security-headers.caddy")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestSecurityHeadersHSTSContract(t *testing.T) {
	body := loadSecurityHeadersCaddy(t)

	wants := []string{
		// Default max-age MUST be 1 year — the HSTS preload-list minimum and
		// the value the pen-test ([SIN-63218](/SIN/issues/SIN-63218)) named.
		`{$HSTS_MAX_AGE:31536000}`,
		// `includeSubDomains` keeps subdomains pinned alongside the apex.
		`includeSubDomains`,
		// `preload` keeps the host eligible for the public preload list.
		`preload`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("deploy/caddy/security-headers.caddy missing required HSTS substring %q", w)
		}
	}

	// Defense-in-depth: the HSTS directive should NOT default to a weak
	// max-age that a careless edit could re-introduce. Reject the 300 / 86400
	// / 2592000 fallback values explicitly so a future operator does not flip
	// the baked default back to a soak-only number without thinking.
	bad := []string{
		`{$HSTS_MAX_AGE:300}`,
		`{$HSTS_MAX_AGE:86400}`,
		`{$HSTS_MAX_AGE:2592000}`,
	}
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("deploy/caddy/security-headers.caddy uses weak HSTS default %q — staging-only values must not be baked in", b)
		}
	}
}

func TestSecurityHeadersComposeDefaults(t *testing.T) {
	// The compose envs ship the same default as the Caddyfile so an operator
	// who never sets HSTS_MAX_AGE in their .env still gets the secure value.
	// If either file's default drifts back to 300, this test fails and the
	// pen-test regression returns silently.
	cases := []struct {
		path string
		want string
	}{
		{"deploy/compose/compose.yml", `HSTS_MAX_AGE: ${HSTS_MAX_AGE:-31536000}`},
		{"deploy/compose/compose.stg.yml", `HSTS_MAX_AGE: ${HSTS_MAX_AGE:-31536000}`},
		{"deploy/compose/.env.example", `HSTS_MAX_AGE=31536000`},
	}
	root := repoRoot(t)
	for _, tc := range cases {
		body, err := os.ReadFile(filepath.Join(root, tc.path))
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s missing %q (Fase 6 HSTS default)", tc.path, tc.want)
		}
	}
}

func TestSecurityHeadersSmokeScriptAsserts(t *testing.T) {
	// The ops-facing smoke script (scripts/check-security-headers.sh) is the
	// runtime checker. Its default expectation MUST match the Caddyfile
	// default; otherwise an operator running it without flags gets a green
	// result even when the edge has drifted back to a weak max-age.
	path := filepath.Join(repoRoot(t), "scripts", "check-security-headers.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	wants := []string{
		`EXPECTED_HSTS_MAX_AGE:-31536000`,
		`max-age=${EXPECTED_HSTS_MAX_AGE}`,
		// The script must also assert `preload` so ops catches a regression
		// where the flag is dropped from the Caddyfile.
		`"preload"`,
	}
	for _, w := range wants {
		if !strings.Contains(string(body), w) {
			t.Errorf("scripts/check-security-headers.sh missing required substring %q", w)
		}
	}
}
