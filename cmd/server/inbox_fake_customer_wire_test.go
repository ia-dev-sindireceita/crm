package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestInboxFakeCustomerFlag_Enabled(t *testing.T) {
	t.Parallel()
	tenantA := uuid.New()
	tenantB := uuid.New()
	cases := []struct {
		name    string
		enabled string
		tenants string
		tenant  uuid.UUID
		want    bool
	}{
		{name: "off by default", enabled: "", tenants: "", tenant: tenantA, want: false},
		{name: "on, empty allowlist enables all tenants", enabled: "1", tenants: "", tenant: tenantA, want: true},
		{name: "on, allowlist includes tenant", enabled: "1", tenants: tenantA.String(), tenant: tenantA, want: true},
		{name: "on, allowlist excludes tenant", enabled: "1", tenants: tenantB.String(), tenant: tenantA, want: false},
		{name: "on, multi-value allowlist", enabled: "1", tenants: tenantB.String() + "," + tenantA.String(), tenant: tenantA, want: true},
		{name: "invalid uuid in allowlist is dropped, not fatal", enabled: "1", tenants: "not-a-uuid," + tenantA.String(), tenant: tenantA, want: true},
		{name: "value other than 1 is off", enabled: "true", tenants: "", tenant: tenantA, want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				switch k {
				case envInboxFakeCustomerEnabled:
					return tc.enabled
				case envInboxFakeCustomerTenants:
					return tc.tenants
				}
				return ""
			}
			flag := newInboxFakeCustomerFlag(getenv)
			got, err := flag.Enabled(context.Background(), tc.tenant)
			if err != nil {
				t.Fatalf("Enabled: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInboxFakeCustomerFlag_NilGetenv(t *testing.T) {
	t.Parallel()
	flag := newInboxFakeCustomerFlag(nil)
	got, err := flag.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Enabled: unexpected error: %v", err)
	}
	if got {
		t.Fatalf("Enabled = true, want false for nil getenv")
	}
}

func TestInboxFakeCustomerRefusedInProd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		appEnv  string
		enabled string
		wantErr bool
	}{
		{name: "production + enabled refuses", appEnv: "production", enabled: "1", wantErr: true},
		{name: "staging-prod + enabled refuses", appEnv: "staging-prod", enabled: "1", wantErr: true},

		{name: "dev + enabled boots", appEnv: "dev", enabled: "1"},
		{name: "staging + enabled boots", appEnv: "staging", enabled: "1"},
		{name: "empty APP_ENV + enabled boots", appEnv: "", enabled: "1"},

		{name: "PRODUCTION upper-case bypasses gate", appEnv: "PRODUCTION", enabled: "1"},
		{name: "prod abbreviation bypasses gate", appEnv: "prod", enabled: "1"},

		{name: "production + disabled boots", appEnv: "production", enabled: ""},
		{name: "staging-prod + disabled boots", appEnv: "staging-prod", enabled: "0"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				switch k {
				case envAppEnv:
					return tc.appEnv
				case envInboxFakeCustomerEnabled:
					return tc.enabled
				}
				return ""
			}
			err := InboxFakeCustomerRefusedInProd(getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected ErrInboxFakeCustomerRefusedInProd, got nil")
				}
				if !errors.Is(err, ErrInboxFakeCustomerRefusedInProd) {
					t.Fatalf("error is not ErrInboxFakeCustomerRefusedInProd: %v", err)
				}
				if !strings.Contains(err.Error(), tc.appEnv) {
					t.Fatalf("error must include offending APP_ENV %q, got %q", tc.appEnv, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestInboxFakeCustomerRefusedInProd_NilGetenv_NilError(t *testing.T) {
	t.Parallel()
	if err := InboxFakeCustomerRefusedInProd(nil); err != nil {
		t.Fatalf("nil getenv: expected nil error, got %v", err)
	}
}
