package vendorassets_test

import (
	"crypto/sha512"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vendor "github.com/pericles-luz/crm/internal/web/vendor"
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

// TestChecksumsFS_ParsesViaProductionAdapter exercises the full
// production code path: ChecksumsFS → vendor.NewFromFS → SRIAttribute.
// If the parser ever rejects a valid line (or vice versa) this gate
// catches the drift before a user hits a 500 on first render.
// SIN-62535 CTO arbitration: the embedded lookup must stay in lockstep
// with the manifest the verify-vendor workflow checks.
func TestChecksumsFS_ParsesViaProductionAdapter(t *testing.T) {
	t.Parallel()
	provider, err := vendor.NewFromFS(vendorassets.ChecksumsFS, vendorassets.ChecksumsManifestPath)
	if err != nil {
		t.Fatalf("NewFromFS: %v", err)
	}
	attr, err := provider.SRIAttribute("htmx/2.0.9/htmx.min.js")
	if err != nil {
		t.Fatalf("SRIAttribute(htmx): %v", err)
	}
	if !strings.Contains(attr, `integrity="sha384-`) {
		t.Fatalf("SRIAttribute missing integrity prefix: %q", attr)
	}
	if !strings.Contains(attr, `crossorigin="anonymous"`) {
		t.Fatalf("SRIAttribute missing crossorigin: %q", attr)
	}
}

// TestChecksumsFS_HashMatchesOnDiskBytes recomputes the sha384 of every
// asset the embedded manifest names from the real bytes in
// web/static/vendor/, and fails if any embedded hash diverges from the
// freshly computed one. This is the Go-side mirror of
// scripts/verify-vendor-checksums.sh: when the bash gate is bypassed
// (e.g. someone runs `go test ./...` without `make verify-vendor`), the
// embedded lookup still catches a drifted manifest before production
// boots.
func TestChecksumsFS_HashMatchesOnDiskBytes(t *testing.T) {
	t.Parallel()
	provider, err := vendor.NewFromFS(vendorassets.ChecksumsFS, vendorassets.ChecksumsManifestPath)
	if err != nil {
		t.Fatalf("NewFromFS: %v", err)
	}
	vendorRoot := findVendorRoot(t)
	// Parse the manifest a second time directly so we can iterate the
	// raw (relPath, hash) pairs the production adapter consumed.
	f, err := vendorassets.ChecksumsFS.Open(vendorassets.ChecksumsManifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	manifest, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	lines := strings.Split(string(manifest), "\n")
	checked := 0
	for i, line := range lines {
		raw := strings.TrimSpace(line)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) != 2 {
			t.Fatalf("manifest line %d malformed: %q", i+1, raw)
		}
		wantHash, relPath := fields[0], fields[1]
		gotHash, err := provider.Hash(relPath)
		if err != nil {
			t.Fatalf("provider.Hash(%q): %v", relPath, err)
		}
		if gotHash != wantHash {
			t.Fatalf("provider hash for %q drifted from manifest: got %q want %q", relPath, gotHash, wantHash)
		}
		assetPath := filepath.Join(vendorRoot, relPath)
		bytes, err := os.ReadFile(assetPath)
		if err != nil {
			t.Fatalf("read vendored asset %q: %v", assetPath, err)
		}
		sum := sha512.Sum384(bytes)
		recomputed := "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
		if recomputed != wantHash {
			t.Fatalf("manifest hash for %q does not match bytes on disk: manifest=%q recomputed=%q", relPath, wantHash, recomputed)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("manifest produced no entries to check")
	}
}

// findVendorRoot walks up from the test working directory to the
// project root and returns the absolute path of web/static/vendor.
// Used by [TestChecksumsFS_HashMatchesOnDiskBytes] to recompute hashes
// from the real bytes on disk.
func findVendorRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 16; i++ {
		candidate := filepath.Join(dir, "web", "static", "vendor")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate web/static/vendor from %q", wd)
	return ""
}
