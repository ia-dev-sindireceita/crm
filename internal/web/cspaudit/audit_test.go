package cspaudit_test

// SIN-63275 audit guard. This test walks every Go and HTML file in
// the CSP-covered scope (internal/web/*, internal/adapter/httpapi/*,
// internal/adapter/transport/http/customdomain/*) and fails if it
// finds an inline <style> or <script> opening tag — i.e. a tag whose
// body is rendered inline, not loaded from `src=` — that does NOT
// carry a `nonce=` attribute.
//
// Why: the CSP middleware (internal/http/middleware/csp) wraps the
// public mux in cmd/server/main.go; every route inherits
// `style-src 'self' 'nonce-…'` without `'unsafe-inline'`. An inline
// <style> without `nonce=` silently breaks the page in production —
// the regression SIN-63275 / SIN-63278 fix.
//
// External-source scripts (`<script src="..."></script>`) are
// allowed by the `'self'` source and do NOT require a nonce; the
// audit accepts them as long as the opening tag carries `src=`.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from the test's cwd until it finds the module's
// `go.mod` file. The cwd is the package directory when `go test`
// runs, so the walk is short (3 hops).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root from %q", dir)
		}
		dir = parent
	}
}

// inlineTagOpenPattern matches `<style …>` or `<script …>` opening
// tags. The multiline-dotall variant lets attributes wrap across
// lines (rare but possible in embedded backtick templates).
var inlineTagOpenPattern = regexp.MustCompile(`(?s)<(style|script)\b([^>]*)>`)

// srcAttrPattern detects an explicit `src=` attribute. When present,
// the script is external and the browser allows it via `'self'`.
var srcAttrPattern = regexp.MustCompile(`\bsrc\s*=`)

// nonceAttrPattern detects the literal `nonce=` attribute (any value)
// on the matched opening tag. The value may be a Go template
// expression (`{{.CSPNonce}}` / `{{$.CSPNonce}}` / `{{cspNonce $}}`)
// or a literal string in test fixtures.
var nonceAttrPattern = regexp.MustCompile(`\bnonce\s*=`)

var scannedExtensions = map[string]bool{
	".go":   true,
	".html": true,
}

// scopeOK enumerates directory prefixes the audit covers. Every
// directory listed is either mounted behind csp.Middleware in
// cmd/server/main.go or otherwise covered by the same policy.
var scopeOK = []string{
	"internal/web/",
	"internal/adapter/httpapi/",
	"internal/adapter/transport/http/customdomain/",
}

func inScope(rel string) bool {
	for _, prefix := range scopeOK {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// excludePathSuffixes lists files we intentionally skip — they
// document the policy or helper in comments rather than emit
// production templates.
var excludePathSuffixes = []string{
	"internal/web/vendor/integrity.go",
}

func TestAudit_AllInlineTagsCarryNonce(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	type offense struct {
		path string
		line int
		tag  string
	}
	var offenses []offense

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !scannedExtensions[ext] {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !inScope(rel) {
			return nil
		}
		for _, suffix := range excludePathSuffixes {
			if strings.HasSuffix(rel, suffix) {
				return nil
			}
		}
		// Skip test files — they hand-craft fixtures that
		// intentionally omit nonce to pin failure modes (XSS escape,
		// fail-closed renders).
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		segments, err := readScannable(path, ext)
		if err != nil {
			return err
		}
		for _, seg := range segments {
			matches := inlineTagOpenPattern.FindAllStringSubmatchIndex(seg.text, -1)
			for _, m := range matches {
				openTag := seg.text[m[0]:m[1]]
				attrs := seg.text[m[4]:m[5]]
				if srcAttrPattern.MatchString(attrs) {
					continue
				}
				if nonceAttrPattern.MatchString(attrs) {
					continue
				}
				offenses = append(offenses, offense{
					path: rel,
					line: seg.lineStart + strings.Count(seg.text[:m[0]], "\n"),
					tag:  strings.ReplaceAll(openTag, "\n", " "),
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenses) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("inline <style>/<script> tag(s) missing nonce attribute under CSP-covered scope:\n")
	for _, o := range offenses {
		b.WriteString("  ")
		b.WriteString(o.path)
		b.WriteString(":")
		b.WriteString(itoa(o.line))
		b.WriteString(" — ")
		b.WriteString(o.tag)
		b.WriteString("\n")
	}
	t.Fatal(b.String())
}

// scanSegment is a (text, line-of-first-byte) pair the audit walks.
// For Go files this is each string literal extracted by go/parser;
// for HTML files it is the whole file body. Splitting Go files this
// way means comments — which routinely document the very pattern
// the audit guards against — never trigger a false positive.
type scanSegment struct {
	text      string
	lineStart int
}

// readScannable returns the inline-template-relevant text segments of
// a file. For Go files we parse the AST and emit one segment per
// string literal (including raw backtick strings, where embedded
// templates live). For HTML files we emit one segment with the full
// body. Anything else returns no segments.
func readScannable(path string, ext string) ([]scanSegment, error) {
	if ext == ".html" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return []scanSegment{{text: string(data), lineStart: 1}}, nil
	}
	if ext != ".go" {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		// A parse failure means we cannot reliably extract literals.
		// Bubble the error so the test surfaces the broken file
		// instead of silently passing.
		return nil, err
	}
	var segments []scanSegment
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			// Unparseable literal — skip, never fail the audit on it.
			return true
		}
		pos := fset.Position(lit.Pos())
		segments = append(segments, scanSegment{
			text:      v,
			lineStart: pos.Line,
		})
		return true
	})
	return segments, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
