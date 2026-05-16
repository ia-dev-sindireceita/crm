// Convention test for the messenger adapter — mirrors the WhatsApp
// equivalent (SIN-62731). The adapter must reach the inbox only
// through the InboundChannel port and MUST NOT import sibling channel
// adapters (whatsapp / webchat / future instagram), nor the
// conversation/message domain entities directly, nor the postgres
// adapter, nor any other internal package that would invert the
// hexagonal dependency direction.
package messenger_test

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
	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp":  "messenger must not import sibling channel adapter",
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram": "messenger must not import sibling channel adapter",
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat":   "messenger must not import sibling channel adapter",
}

// allowedInboxImports whitelists the only inbox sub-import permitted
// (the package-level types: InboundChannel, InboundEvent, sentinels).
var allowedInboxImports = map[string]struct{}{
	"github.com/pericles-luz/crm/internal/inbox": {},
}

func TestConvention_NoDomainLeak(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
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
