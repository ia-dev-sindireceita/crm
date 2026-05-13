package customdomain

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"sync"

	vendorintegrity "github.com/pericles-luz/crm/internal/web/vendor"
	vendorassets "github.com/pericles-luz/crm/web/static/vendor"
)

// templateFS holds the embedded *.html sources. The handler parses them
// once into one *template.Template and reuses it for every render.
//
//go:embed templates/*.html
var templateFS embed.FS

var (
	tmplOnce sync.Once
	tmpl     *template.Template
	tmplErr  error

	// newVendorIntegrity is the seam used by tests to inject a stub
	// provider. Production builds use the embedded CHECKSUMS.txt; the
	// adapter lives in internal/web/vendor so the template package only
	// depends on the VendorIntegrity port.
	newVendorIntegrity = func() (vendorintegrity.VendorIntegrity, error) {
		return vendorintegrity.NewFromFS(vendorassets.ChecksumsFS, vendorassets.ChecksumsManifestPath)
	}

	// requiredVendorAssets enumerates every vendored relpath referenced
	// by templates in this package. loadTemplates verifies each path
	// exists in the parsed manifest and panics at startup if any are
	// missing — this turns "I forgot to vendor the file" or "I typo'd
	// the relpath" into a server-boot crash instead of a 500 on first
	// request. Add a new entry here whenever base.html (or a sibling)
	// gains a `{{ vendorSRI "..." }}` reference.
	requiredVendorAssets = []string{
		"htmx/2.0.9/htmx.min.js",
	}
)

// buildFuncMap returns the FuncMap registered with the embedded
// templates. Hoisted out of the once-gated parser so internal tests
// can exercise the vendorSRI helper directly with a stub provider.
func buildFuncMap(provider vendorintegrity.VendorIntegrity) template.FuncMap {
	return template.FuncMap{
		// vendorSRI renders the integrity + crossorigin attribute pair
		// for a vendored JS asset. Returning template.HTMLAttr lets
		// html/template emit the attribute fragment verbatim — the
		// caller controls the relPath, and the hash itself is base64
		// sha384 so it cannot inject markup.
		"vendorSRI": func(relPath string) (template.HTMLAttr, error) {
			attr, err := provider.SRIAttribute(relPath)
			if err != nil {
				return "", err
			}
			return template.HTMLAttr(attr), nil
		},
	}
}

// loadTemplates parses the embedded templates with the boundary's
// custom funcs. Called once on the first render — the package init has
// to stay side-effect free for the test binary.
//
// An unknown asset reference (an entry in [requiredVendorAssets] that
// is not present in the parsed manifest) is a programmer error and
// panics. The CTO's SIN-62535 arbitration is explicit: missing vendor
// references must crash at startup rather than 500 on first request.
func loadTemplates() (*template.Template, error) {
	tmplOnce.Do(func() {
		provider, err := newVendorIntegrity()
		if err != nil {
			tmplErr = fmt.Errorf("customdomain: vendor integrity: %w", err)
			return
		}
		for _, relPath := range requiredVendorAssets {
			if _, hashErr := provider.SRIAttribute(relPath); hashErr != nil {
				panic(fmt.Sprintf("customdomain: vendor manifest missing required asset %q: %v", relPath, hashErr))
			}
		}
		t := template.New("base").Funcs(buildFuncMap(provider))
		t, err = t.ParseFS(templateFS, "templates/*.html")
		if err != nil {
			tmplErr = fmt.Errorf("customdomain: parse templates: %w", err)
			return
		}
		tmpl = t
	})
	return tmpl, tmplErr
}

// renderTemplate executes name into w. Errors are returned to the
// caller; the handler is responsible for crafting an error response.
func renderTemplate(w io.Writer, name string, data any) error {
	t, err := loadTemplates()
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, name, data)
}
