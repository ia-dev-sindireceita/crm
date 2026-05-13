package vendor_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	vendor "github.com/pericles-luz/crm/internal/web/vendor"
)

// fakeReader returns the first read happily, then a sentinel error so
// we can exercise the bufio.Scanner failure path in Parse.
type fakeReader struct {
	chunk     string
	delivered bool
	failErr   error
}

func (f *fakeReader) Read(p []byte) (int, error) {
	if !f.delivered {
		n := copy(p, f.chunk)
		f.delivered = true
		return n, nil
	}
	return 0, f.failErr
}

func TestParse_KnownPathReturnsExpectedHash(t *testing.T) {
	t.Parallel()
	const manifest = "" +
		"sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  htmx/2.0.9/htmx.min.js\n" +
		"sha384-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB  alpinejs/3.14.9/alpinejs.min.js\n"
	p, err := vendor.Parse(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	got, err := p.Hash("htmx/2.0.9/htmx.min.js")
	if err != nil {
		t.Fatalf("Hash: unexpected error: %v", err)
	}
	const want = "sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got != want {
		t.Fatalf("Hash mismatch: got %q want %q", got, want)
	}
}

func TestParse_SkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	const manifest = "" +
		"# comment line\n" +
		"\n" +
		"   \n" +
		"sha384-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC  only/one.js\n"
	p, err := vendor.Parse(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if _, err := p.Hash("only/one.js"); err != nil {
		t.Fatalf("Hash: unexpected error: %v", err)
	}
}

func TestParse_RejectsMalformedLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			name:    "single field",
			input:   "sha384-aaaaaaaaaaaaaaaaaaaaaa\n",
			wantSub: "malformed manifest at line 1",
		},
		{
			name:    "three fields",
			input:   "sha384-aaaaaaaaaaaaaaaaaaaaaa  path/one.js   extra\n",
			wantSub: "malformed manifest at line 1",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := vendor.Parse(strings.NewReader(tc.input))
			if err == nil {
				t.Fatalf("Parse: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Parse: error %q missing substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestParse_RejectsNonSha384Prefix(t *testing.T) {
	t.Parallel()
	const manifest = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  path/one.js\n"
	_, err := vendor.Parse(strings.NewReader(manifest))
	if err == nil {
		t.Fatal("Parse: expected error for non-sha384 prefix")
	}
	if !strings.Contains(err.Error(), "expected sha384- prefix") {
		t.Fatalf("Parse: wrong error: %v", err)
	}
}

func TestParse_RejectsDuplicateEntries(t *testing.T) {
	t.Parallel()
	const manifest = "" +
		"sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  path/one.js\n" +
		"sha384-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB  path/one.js\n"
	_, err := vendor.Parse(strings.NewReader(manifest))
	if err == nil {
		t.Fatal("Parse: expected duplicate-entry error")
	}
	if !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("Parse: wrong error: %v", err)
	}
}

func TestParse_RejectsEmptyManifest(t *testing.T) {
	t.Parallel()
	_, err := vendor.Parse(strings.NewReader("\n# only comments\n\n"))
	if err == nil {
		t.Fatal("Parse: expected error for empty manifest")
	}
	if !strings.Contains(err.Error(), "no entries") {
		t.Fatalf("Parse: wrong error: %v", err)
	}
}

func TestParse_ReturnsScannerError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	_, err := vendor.Parse(&fakeReader{
		chunk:   "sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  path/one.js\n",
		failErr: sentinel,
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("Parse: expected wrapped sentinel, got %v", err)
	}
}

func TestProvider_HashUnknownAssetWrapsSentinel(t *testing.T) {
	t.Parallel()
	const manifest = "sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  known.js\n"
	p, err := vendor.Parse(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = p.Hash("missing.js")
	if !errors.Is(err, vendor.ErrUnknownAsset) {
		t.Fatalf("Hash(missing): want ErrUnknownAsset, got %v", err)
	}
}

func TestProvider_SRIAttributeFormatsKnownAsset(t *testing.T) {
	t.Parallel()
	const want = `integrity="sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" crossorigin="anonymous"`
	const manifest = "sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  known.js\n"
	p, err := vendor.Parse(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := p.SRIAttribute("known.js")
	if err != nil {
		t.Fatalf("SRIAttribute: %v", err)
	}
	if got != want {
		t.Fatalf("SRIAttribute mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestProvider_SRIAttributeUnknownAssetPropagates(t *testing.T) {
	t.Parallel()
	const manifest = "sha384-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  known.js\n"
	p, err := vendor.Parse(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = p.SRIAttribute("missing.js")
	if !errors.Is(err, vendor.ErrUnknownAsset) {
		t.Fatalf("SRIAttribute(missing): want ErrUnknownAsset, got %v", err)
	}
}

func TestNewFromFS_HappyPathAndMissing(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"CHECKSUMS.txt": &fstest.MapFile{
			Data: []byte("sha384-DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD  htmx/2.0.9/htmx.min.js\n"),
		},
	}
	p, err := vendor.NewFromFS(fsys, "CHECKSUMS.txt")
	if err != nil {
		t.Fatalf("NewFromFS: %v", err)
	}
	if _, err := p.Hash("htmx/2.0.9/htmx.min.js"); err != nil {
		t.Fatalf("Hash: unexpected error: %v", err)
	}

	if _, err := vendor.NewFromFS(fsys, "DOES-NOT-EXIST"); err == nil {
		t.Fatal("NewFromFS: expected error for missing manifest")
	}
}

// Compile-time assurance that *Provider implements VendorIntegrity. If
// the interface ever gains a method this will fail loudly here.
var _ vendor.VendorIntegrity = (*vendor.Provider)(nil)

// Quiet the unused-import warning for io if the file ever stops using
// it directly; keeping the test scaffolding obvious.
var _ io.Reader = (*fakeReader)(nil)
