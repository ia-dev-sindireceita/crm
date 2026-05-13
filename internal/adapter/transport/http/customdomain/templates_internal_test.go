package customdomain

import (
	"errors"
	"html/template"
	"strings"
	"testing"
)

// stubProvider is the in-package test double for vendorintegrity.
// VendorIntegrity. The default behaviour mirrors the production
// adapter; tests opt into the failing path by setting err.
type stubProvider struct {
	attr string
	err  error
}

func (s *stubProvider) SRIAttribute(_ string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.attr, nil
}

func TestBuildFuncMap_VendorSRI_HappyPath(t *testing.T) {
	t.Parallel()
	const want = `integrity="sha384-XX" crossorigin="anonymous"`
	stub := &stubProvider{attr: want}

	tmpl := template.Must(template.New("t").Funcs(buildFuncMap(stub)).
		Parse(`<script {{ vendorSRI "anything.js" }}></script>`))

	var buf strings.Builder
	if err := tmpl.Execute(&buf, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("output missing attribute pair:\n%s", buf.String())
	}
}

func TestBuildFuncMap_VendorSRI_PropagatesProviderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider broken")
	stub := &stubProvider{err: sentinel}

	tmpl := template.Must(template.New("t").Funcs(buildFuncMap(stub)).
		Parse(`<script {{ vendorSRI "missing.js" }}></script>`))

	var buf strings.Builder
	err := tmpl.Execute(&buf, nil)
	if err == nil {
		t.Fatal("Execute: expected error from provider")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Execute: error %v missing wrapped sentinel %v", err, sentinel)
	}
}
