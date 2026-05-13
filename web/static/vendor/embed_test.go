package vendorassets_test

import (
	"io"
	"strings"
	"testing"

	vendorassets "github.com/pericles-luz/crm/web/static/vendor"
)

// TestChecksumsFS_EmbedsManifest asserts the build embedded the real
// CHECKSUMS.txt and that it lists the assets actually consumed by the
// templates. This is the live wiring that proves SIN-62535's defence
// works: if the manifest disappears or the htmx path stops being
// listed, the SRI helper can't render the integrity attribute and the
// browser would silently lose its verification gate.
func TestChecksumsFS_EmbedsManifest(t *testing.T) {
	t.Parallel()
	f, err := vendorassets.ChecksumsFS.Open(vendorassets.ChecksumsManifestPath)
	if err != nil {
		t.Fatalf("open %s: %v", vendorassets.ChecksumsManifestPath, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		// htmx is the only currently-vendored bundle as of SIN-62536
		// (Alpine.js dropped). Add new entries here when vendoring
		// brings in additional bundles so the embed gate keeps proving
		// the manifest is wired through.
		"htmx/2.0.9/htmx.min.js",
		"sha384-",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("CHECKSUMS.txt missing %q\nbody:\n%s", want, body)
		}
	}
}
