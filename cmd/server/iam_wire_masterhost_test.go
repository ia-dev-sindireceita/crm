package main

// SIN-65076 — wireup tests for masterConsoleHost, the helper that reads
// MASTER_CONSOLE_HOST and feeds httpapi.Deps.MasterHost. Before this
// wire the field was never assigned in production, so the master
// console host was unreachable and csrfAllowedHosts never included it.
//
// The tests pin the contract the boot path relies on:
//   - set            → value propagates verbatim,
//   - unset / blank  → empty string (graceful degradation, no panic),
//   - surrounding whitespace is trimmed so a stray newline in a .env
//     file doesn't produce a host that never matches the Origin header.
//
// The downstream effect — csrfAllowedHosts including the MasterHost in
// the CSRF Origin/Referer allowlist when it is non-empty — is already
// covered end-to-end through the router by
// TestRouter_CSRF_LogoutOriginAllowlist_AcceptsMasterHost in
// internal/adapter/httpapi/router_csrf_test.go. This file only needs to
// prove the previously-missing wire that makes MasterHost non-empty.

import (
	"testing"
)

func TestMasterConsoleHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  string
		want string
	}{
		{name: "set", env: "master.crm.crm.someu.com.br", want: "master.crm.crm.someu.com.br"},
		{name: "unset", env: "", want: ""},
		{name: "blank whitespace", env: "   ", want: ""},
		{name: "trims surrounding whitespace", env: "  master.crm.local\n", want: "master.crm.local"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := masterConsoleHost(func(k string) string {
				if k == envMasterConsoleHost {
					return tc.env
				}
				return ""
			})
			if got != tc.want {
				t.Fatalf("masterConsoleHost(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestMasterConsoleHost_OnlyReadsItsOwnVar guards against the helper
// accidentally keying off the wrong env var: a getenv that returns a
// value for every other key but empty for MASTER_CONSOLE_HOST must
// still degrade to "".
func TestMasterConsoleHost_OnlyReadsItsOwnVar(t *testing.T) {
	t.Parallel()
	got := masterConsoleHost(func(k string) string {
		if k == envMasterConsoleHost {
			return ""
		}
		return "other.example.com"
	})
	if got != "" {
		t.Fatalf("masterConsoleHost = %q, want empty when %s unset", got, envMasterConsoleHost)
	}
}
