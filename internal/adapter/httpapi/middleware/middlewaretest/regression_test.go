package middlewaretest_test

// SIN-63978 / SIN-63956 §F3 regression guard.
//
// The point of the testing.TB-gated middlewaretest package is to make
// the impersonation envelope installer unreachable from production
// code. The compile-time gate fires only if a forbidden caller
// actually tries to invoke testing.TB-free overloads, but a future
// refactor could re-export an unguarded sibling on the middleware
// package and accidentally re-open the hole.
//
// This test walks the internal/ tree at module root and asserts that:
//
//  1. The old `WithActiveImpersonationForTest` symbol is absent
//     entirely (it was renamed, not aliased).
//  2. The new package-private hook `InstallActiveImpersonationForTest`
//     is referenced ONLY from non-_test.go production files inside the
//     middleware package (its own definition) and from any file under
//     the middlewaretest/ subpackage.
//
// Failure of either invariant means a future change exposed the
// envelope installer outside of test scope, which would defeat the
// hardening that SIN-63978 puts in place.

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProductionDoesNotReferenceTestHelper(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	internal := filepath.Join(root, "internal")

	type finding struct {
		path   string
		symbol string
		line   int
	}
	var findings []finding

	const (
		oldSymbol = "WithActiveImpersonationForTest"
		newSymbol = "InstallActiveImpersonationForTest"
	)

	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)

		isMiddlewaretest := strings.HasPrefix(filepath.ToSlash(rel), "internal/adapter/httpapi/middleware/middlewaretest/")
		isMiddlewareDef := filepath.ToSlash(rel) == "internal/adapter/httpapi/middleware/impersonation_session.go"
		isTestFile := strings.HasSuffix(rel, "_test.go")

		// Old symbol (renamed away): allowed in _test.go files and
		// inside the middlewaretest/ subpackage (doc references in
		// either are fine). Production references anywhere else are
		// a regression.
		if idx, line := findSymbol(text, oldSymbol); idx >= 0 {
			if !isMiddlewaretest && !isTestFile {
				findings = append(findings, finding{path: rel, symbol: oldSymbol, line: line})
			}
		}

		// New symbol may appear in:
		//   - its own definition (impersonation_session.go) in the
		//     middleware package (production file, no _test.go).
		//   - the middlewaretest subpackage (any .go file).
		//   - _test.go files anywhere.
		// Anywhere else is a regression.
		if idx, line := findSymbol(text, newSymbol); idx >= 0 {
			if !isMiddlewaretest && !isMiddlewareDef && !isTestFile {
				findings = append(findings, finding{path: rel, symbol: newSymbol, line: line})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	if len(findings) > 0 {
		var b strings.Builder
		b.WriteString("forbidden references to impersonation test helper found in production code:\n")
		for _, f := range findings {
			b.WriteString("  - ")
			b.WriteString(f.path)
			b.WriteString(":")
			b.WriteString(itoa(f.line))
			b.WriteString(" — ")
			b.WriteString(f.symbol)
			b.WriteString("\n")
		}
		b.WriteString("\nThe helper must remain reachable only via middlewaretest.WithActiveImpersonation (testing.TB-gated).")
		t.Fatal(b.String())
	}
}

// findSymbol returns the byte index and 1-based line number of the
// first occurrence of name as a whole identifier in text, or -1, 0
// when not present.
func findSymbol(text, name string) (int, int) {
	start := 0
	for {
		i := strings.Index(text[start:], name)
		if i < 0 {
			return -1, 0
		}
		abs := start + i
		// Whole-identifier check: the byte before and after must
		// not be an identifier character.
		if (abs == 0 || !isIdent(text[abs-1])) && (abs+len(name) >= len(text) || !isIdent(text[abs+len(name)])) {
			return abs, 1 + strings.Count(text[:abs], "\n")
		}
		start = abs + len(name)
	}
}

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// moduleRoot walks up from this test file's location until it finds a
// directory containing go.mod. Using runtime.Caller keeps the test
// portable across CI working directories (go test runs with cwd set
// to the package, not the module root).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}
