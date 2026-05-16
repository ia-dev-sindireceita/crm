// Convention test for AC #4 (SIN-62796): the Instagram adapter must
// reach the inbox only through the InboundChannel port and must NOT
// import its sibling Meta-channel packages (whatsapp / messenger /
// webchat) — every cross-channel concern lives in metashared (F2-02).
//
// The check is a go/parser walk over the non-test files in this
// package. We compile the per-file import set, then forbid a small
// list of paths.
package instagram_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var forbiddenImports = map[string]string{
	"github.com/pericles-luz/crm/internal/inbox/usecase":              "use the inbox.InboundChannel port, not the use-case struct directly",
	"github.com/pericles-luz/crm/internal/adapter/db":                 "adapter must not import db/* — wire concrete stores at the composition root",
	"github.com/pericles-luz/crm/internal/adapter/store":              "adapter must not import store/* — wire concrete stores at the composition root",
	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp":  "instagram must not import sibling channel whatsapp",
	"github.com/pericles-luz/crm/internal/adapter/channels/messenger": "instagram must not import sibling channel messenger",
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat":   "instagram must not import sibling channel webchat",
	"github.com/pericles-luz/crm/internal/adapter/channel/whatsapp":   "legacy whatsapp adapter is off-limits to instagram",
}

// allowedInboxImports whitelists the only inbox sub-import permitted
// (the package-level types: InboundChannel, InboundEvent, sentinels).
var allowedInboxImports = map[string]struct{}{
	"github.com/pericles-luz/crm/internal/inbox": {},
}

func TestConvention_NoDomainOrSiblingLeak(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(wd)
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
		path := filepath.Join(wd, name)
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
