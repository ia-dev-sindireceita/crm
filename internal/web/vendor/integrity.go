// Package vendor exposes the [VendorIntegrity] port and an
// fs.FS-backed adapter that parses web/static/vendor/CHECKSUMS.txt and
// answers Subresource-Integrity attribute lookups for HTML templates.
//
// SIN-62535 — defence-in-depth for the vendored HTMX/Alpine bundles
// (SIN-62284). The HTMX use-site at internal/adapter/transport/http/
// customdomain/templates/base.html consults this package via the
// vendorSRI template helper so the browser re-verifies the bytes it
// executes, catching any in-flight mutation between the origin and the
// client (proxies, asset rewriters, future CDN edges).
package vendor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// VendorIntegrity is the port that HTML templates use to render the SRI
// attribute fragment for a vendored asset reference. Adapters parse the
// CHECKSUMS.txt manifest at startup; the helper returns a ready-to-inline
// `integrity="sha384-..." crossorigin="anonymous"` substring.
type VendorIntegrity interface {
	SRIAttribute(relPath string) (string, error)
}

// ErrUnknownAsset is returned by [Provider.Hash] and
// [Provider.SRIAttribute] when the requested relPath is absent from the
// loaded manifest. Callers can branch on errors.Is.
var ErrUnknownAsset = errors.New("vendor: unknown asset")

// Provider answers integrity lookups from an in-memory map populated at
// construction time. Safe for concurrent reads; no further locking
// because the map is never mutated after [NewFromFS] returns.
type Provider struct {
	hashes map[string]string
}

// NewFromFS loads the checksum manifest at manifestPath from fsys and
// returns a Provider. The manifest is parsed eagerly — any malformed
// line, duplicate entry, or non-sha384 prefix fails fast so the program
// cannot silently boot with a half-loaded manifest.
func NewFromFS(fsys fs.FS, manifestPath string) (*Provider, error) {
	f, err := fsys.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("vendor: open manifest %q: %w", manifestPath, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads a CHECKSUMS.txt-style manifest from r. Each non-blank,
// non-comment line must have the shape `sha384-<base64>  <relpath>`,
// matching the output of scripts/verify-vendor-checksums.sh.
func Parse(r io.Reader) (*Provider, error) {
	hashes := make(map[string]string)
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) != 2 {
			return nil, fmt.Errorf("vendor: malformed manifest at line %d: %q", line, raw)
		}
		hash, relPath := fields[0], fields[1]
		if !strings.HasPrefix(hash, "sha384-") {
			return nil, fmt.Errorf("vendor: manifest line %d: expected sha384- prefix, got %q", line, hash)
		}
		if _, dup := hashes[relPath]; dup {
			return nil, fmt.Errorf("vendor: manifest line %d: duplicate entry for %q", line, relPath)
		}
		hashes[relPath] = hash
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("vendor: scan manifest: %w", err)
	}
	if len(hashes) == 0 {
		return nil, errors.New("vendor: manifest contained no entries")
	}
	return &Provider{hashes: hashes}, nil
}

// Hash returns the raw sha384-prefixed integrity value for the asset at
// relPath, or wraps [ErrUnknownAsset] when the manifest has no entry.
func (p *Provider) Hash(relPath string) (string, error) {
	if h, ok := p.hashes[relPath]; ok {
		return h, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownAsset, relPath)
}

// SRIAttribute renders the attribute fragment that the template helper
// inlines between the `src` and `nonce` attributes of a <script> tag:
// `integrity="sha384-..." crossorigin="anonymous"`. The double quotes
// are embedded so the HTML stays parser-friendly and html/template
// emits the value verbatim (the caller wraps the result in
// template.HTMLAttr).
func (p *Provider) SRIAttribute(relPath string) (string, error) {
	h, err := p.Hash(relPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`integrity=%q crossorigin="anonymous"`, h), nil
}
