package tenancy_test

import (
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/tenancy"
)

func TestParseHost_Variants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		wantSub  string
		wantRoot string
		wantErr  error
	}{
		{name: "platform_subdomain_local", host: "acme.crm.local", wantSub: "acme", wantRoot: "crm.local"},
		{name: "platform_subdomain_prod", host: "globex.crm.example.com", wantSub: "globex", wantRoot: "crm.example.com"},
		{name: "platform_root_no_subdomain", host: "crm.local", wantRoot: "crm.local", wantErr: tenancy.ErrNoTenantSubdomain},
		{name: "custom_domain_candidate", host: "acme.com.br", wantRoot: "acme.com.br"},
		{name: "with_port_stripped", host: "acme.crm.local:8443", wantSub: "acme", wantRoot: "crm.local"},
		{name: "uppercase_normalised", host: "ACME.CRM.LOCAL", wantSub: "acme", wantRoot: "crm.local"},
		{name: "trailing_dot_stripped", host: "acme.crm.local.", wantSub: "acme", wantRoot: "crm.local"},
		{name: "empty_host", host: "", wantErr: tenancy.ErrEmptyHost},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSub, gotRoot, err := tenancy.ParseHost(tc.host)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseHost(%q) err = %v, want %v", tc.host, err, tc.wantErr)
			}
			if gotSub != tc.wantSub {
				t.Errorf("subdomain = %q, want %q", gotSub, tc.wantSub)
			}
			if gotRoot != tc.wantRoot {
				t.Errorf("root = %q, want %q", gotRoot, tc.wantRoot)
			}
		})
	}
}
