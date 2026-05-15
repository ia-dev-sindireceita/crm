// Convention test for AC #4 (SIN-62791): metashared MUST NOT import
// any specific channel adapter (whatsapp / instagram / messenger) nor
// any domain package (inbox, contacts). It is infrastructure shared
// across Meta channels; the dependency direction is channel → shared,
// never the reverse.
package metashared_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenPrefixes lists package-path prefixes metashared MUST NOT
// import. The list intentionally over-approximates the lens in the
// task spec: "_meta_shared não importa nenhum canal específico" plus
// the hexagonal rule "não pode importar internal/inbox ou
// internal/contacts".
var forbiddenPrefixes = []string{
	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp",
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram",
	"github.com/pericles-luz/crm/internal/adapter/channels/messenger",
	"github.com/pericles-luz/crm/internal/adapter/channel/", // sibling singular tree (sender/meta adapters)
	"github.com/pericles-luz/crm/internal/inbox",
	"github.com/pericles-luz/crm/internal/contacts",
}

func TestConvention_NoChannelOrDomainImports(t *testing.T) {
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
			for _, bad := range forbiddenPrefixes {
				if strings.HasPrefix(ipath, bad) {
					t.Errorf("%s imports forbidden %q (matches prefix %q): metashared is infra, channels depend on it not the other way around",
						name, ipath, bad)
				}
			}
		}
	}
}
