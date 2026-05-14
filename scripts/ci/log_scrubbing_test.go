// Package ci_test contains the CI log-scrubbing gate (ADR 0004 §5).
//
// It runs a subset of the application test suite with stdout/stderr
// captured and then checks for common PII / secret patterns. A match
// fails CI with a message pointing at the offending line.
//
// Run with: go test ./scripts/ci/... or `make test` (included by default).
//
// NOTE: this test file lives under scripts/ci/ so it can be excluded
// from normal coverage measurement while still participating in `go test
// ./...`. It has no build tag so it is always compiled.
package ci_test

import (
	"bytes"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// leakPatterns is the set of regexps that MUST NOT appear in any log
// line produced during testing. Each entry carries a human-readable
// name used in the failure message.
var leakPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{
		"plain-password-correct-horse",
		regexp.MustCompile(`correct horse battery staple`),
	},
	{
		"plain-password-password123",
		regexp.MustCompile(`(?i)password123`),
	},
	{
		"plain-password-p@ssw0rd",
		regexp.MustCompile(`p@ssw0rd`),
	},
	{
		"jwt-pattern",
		// Three base64url segments separated by dots — a JWT signature.
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	},
	{
		"cpf-pattern",
		// CPF: digits with optional separators 000.000.000-00 or 00000000000
		regexp.MustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b`),
	},
	{
		"email-in-info-debug",
		// Bare e-mail address. Logs are JSON; only flag lines at info/debug level.
		regexp.MustCompile(`"level":"(INFO|DEBUG)"[^}]*"[^"]*@[^"]*\.[^"]{2,}"[^}]*}`),
	},
	{
		"phone-br-pattern",
		// Brazilian mobile/landline: +55 (XX) XXXXX-XXXX or similar
		regexp.MustCompile(`\+55\s?\(?\d{2}\)?\s?\d{4,5}-?\d{4}`),
	},
}

// TestNoSecretLeaksInTestOutput runs the internal/log and internal/obs
// package tests with verbose output captured and checks for PII leaks.
func TestNoSecretLeaksInTestOutput(t *testing.T) {
	t.Parallel()

	// Run the targeted packages. We capture combined stdout+stderr so
	// that any slog output emitted during tests is visible.
	cmd := exec.Command("go", "test", "-v", "-count=1",
		"github.com/pericles-luz/crm/internal/log",
		"github.com/pericles-luz/crm/internal/obs",
	)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Logf("go test output:\n%s", out.String())
		t.Fatalf("test suite failed: %v", err)
	}

	checkOutput(t, out.String())
}

// checkOutput is separated so it can be unit-tested directly with
// injected strings — the CI gate itself uses it on real test output.
func checkOutput(t *testing.T, output string) {
	t.Helper()
	for _, p := range leakPatterns {
		lines := strings.Split(output, "\n")
		for lineNum, line := range lines {
			if p.pattern.MatchString(line) {
				t.Errorf("[log-scrubbing] LEAK DETECTED pattern=%q line=%d content=%q — "+
					"a test is emitting a raw secret/PII. See ADR 0004 for remediation.",
					p.name, lineNum+1, truncate(line, 200))
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
