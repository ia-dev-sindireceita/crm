// Convention test for AC #5 (SIN-62731): the WhatsApp adapter must
// reach the inbox only through the InboundChannel port. It MUST NOT
// import the conversation or message domain entities directly, nor
// the postgres adapter, nor any other internal package that would
// invert the hexagonal dependency direction.
//
// The check is a go/parser walk over the non-test files in this
// package. We compile the per-file import set, then forbid a small
// list of paths. The test is intentionally textual rather than
// requiring a separate go/analysis pass because:
//
//   - It needs to pass on a developer workstation with go test alone.
//   - The forbidden-import list is tiny (4 entries) so a regex/AST
//     approach is overkill.
//
// Adding a new entry to forbiddenImports is the right reaction the
// moment a follow-up PR introduces an adapter or domain leak we did
// not anticipate here.
package whatsapp_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var forbiddenImports = map[string]string{
	"github.com/pericles-luz/crm/internal/inbox/usecase": "use the inbox.InboundChannel port, not the use-case struct directly",
	"github.com/pericles-luz/crm/internal/adapter/db":    "adapter must not import db/* — wire concrete stores at the composition root",
	"github.com/pericles-luz/crm/internal/adapter/store": "adapter must not import store/* — wire concrete stores at the composition root",
}

// allowedInboxSubpaths whitelists the only inbox sub-import permitted
// (the package-level types: InboundChannel, InboundEvent, sentinels).
// Importing the conversation/message entity files directly is a
// hexagonal violation. We approximate "files" by checking that the
// import path does NOT end with /usecase or /<entity>.
var allowedInboxImports = map[string]struct{}{
	"github.com/pericles-luz/crm/internal/inbox": {},
}

func TestConvention_NoDomainLeak(t *testing.T) {
	t.Parallel()
	dir, err := packageDir(t)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: parse: %v", path, err)
		}
		for _, imp := range f.Imports {
			ipath := strings.Trim(imp.Path.Value, "\"")
			if reason, bad := forbiddenImports[ipath]; bad {
				t.Errorf("%s imports forbidden %q: %s", name, ipath, reason)
			}
			if strings.HasPrefix(ipath, "github.com/pericles-luz/crm/internal/inbox") {
				if _, ok := allowedInboxImports[ipath]; !ok {
					t.Errorf("%s imports inbox sub-package %q — only \"internal/inbox\" (port types) is allowed",
						name, ipath)
				}
			}
		}
	}
}

func packageDir(t *testing.T) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd, nil
}
