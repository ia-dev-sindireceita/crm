// Package forbidwebboundary implements a go/analysis pass that fails CI
// when a package under internal/web/... imports the inbox domain root
// (internal/inbox) directly. The hexagonal boundary requires web/HTMX
// handlers to call inbox use cases (internal/inbox/usecase) and consume
// their projected view shapes; reaching past the use case into the
// aggregate root breaks the "handlers call use cases" rule documented
// in the SIN-62735 task (AC #4 "Lint custom verde").
//
// The override marker `// forbidwebboundary:ok <reason>` silences the
// rule for one import line when the reason is supplied. Bare markers
// are rejected so the override review surface stays small.
package forbidwebboundary

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the go/analysis pass that backs the `forbidwebboundary`
// linter. Wire it up in CI with:
//
//	go vet -vettool=$(which forbidwebboundary) ./internal/web/...
var Analyzer = &analysis.Analyzer{
	Name: "forbidwebboundary",
	Doc:  "reports packages under internal/web/... that import internal/inbox (domain root) directly; route inbox access through internal/inbox/usecase (SIN-62735)",
	Run:  run,
}

// forbiddenExact lists fully-qualified import paths the rule rejects
// from internal/web/...-rooted packages.
var forbiddenExact = map[string]struct{}{
	"github.com/pericles-luz/crm/internal/inbox": {},
}

// scopedPkgPrefixes are the package-path prefixes whose Go files trigger
// the analyzer. The rule is intentionally narrow: only internal/web/...
// is checked. Other packages (use cases, adapters, the domain itself)
// import internal/inbox legitimately.
var scopedPkgPrefixes = []string{
	"github.com/pericles-luz/crm/internal/web",
}

// overrideMarker silences the rule for one import line when followed by
// a non-empty justification. Bare markers are rejected.
const overrideMarker = "forbidwebboundary:ok"

func run(pass *analysis.Pass) (any, error) {
	if !isScoped(pass.Pkg.Path()) {
		return nil, nil
	}
	for _, file := range pass.Files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !isForbidden(path) {
				continue
			}
			if hasOverride(pass.Fset, file, imp) {
				continue
			}
			pass.Reportf(imp.Pos(),
				"forbidden import %q from internal/web/...; HTMX handlers must call use cases in internal/inbox/usecase, not the inbox domain root (SIN-62735)",
				path)
		}
	}
	return nil, nil
}

// isForbidden reports whether path is a forbidden import for the scoped
// packages. Currently a single domain root, but the data shape keeps the
// door open for adding sibling roots without restructuring the loop.
func isForbidden(path string) bool {
	_, ok := forbiddenExact[path]
	return ok
}

// isScoped reports whether the package falls under the analyzer's
// jurisdiction. The `_test` suffix on external test packages is stripped
// so external tests share the rule with the package they cover.
func isScoped(pkgPath string) bool {
	pkgPath = strings.TrimSuffix(pkgPath, "_test")
	for _, p := range scopedPkgPrefixes {
		if pkgPath == p || strings.HasPrefix(pkgPath, p+"/") {
			return true
		}
	}
	return false
}

// hasOverride reports whether the import is silenced by a
// // forbidwebboundary:ok <reason> comment on the same line as the
// import or the line directly above. A bare marker without a trailing
// reason is rejected so overrides must include a justification.
func hasOverride(fset *token.FileSet, file *ast.File, imp *ast.ImportSpec) bool {
	importPos := fset.Position(imp.Pos())
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, overrideMarker) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(text, overrideMarker))
			if rest == "" {
				continue
			}
			cPos := fset.Position(c.Pos())
			if cPos.Filename != importPos.Filename {
				continue
			}
			if cPos.Line == importPos.Line || cPos.Line+1 == importPos.Line {
				return true
			}
		}
	}
	return false
}
