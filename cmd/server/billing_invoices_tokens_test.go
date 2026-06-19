package main

// SIN-65228 (Peitho hygiene) — tokens-only guard for the billing invoices
// stylesheet.
//
// The SIN-63944 rewrite (#365) reintroduced bare rem/px/radius literals,
// dead `var(--token, #hex)` fallbacks, and — worse — paired the
// "text ON the fill" tokens (--color-warning-text / --color-success-text,
// which are white/near-black) with the pale --color-*-surface backgrounds,
// rendering invisible white-on-pale pill text in light mode. This guard
// fails if any of that creeps back: it scans the declaration body (comments
// stripped) for raw hex and for the mispaired text-on-fill tokens, and
// asserts the AA text-on-surface tokens are the ones in use.
//
// Companion to TestBillingInvoicesStylesheet_ServedAsCSS (the existing
// existence/served-as-css guard in billing_invoices_css_static_test.go).

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	cssCommentRE   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssBareHexRE   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	cssTokenFallbk = regexp.MustCompile(`var\(\s*--[a-z0-9-]+\s*,`)
)

func readBillingInvoicesCSS(t *testing.T) string {
	t.Helper()
	// cmd/server lives two levels below the repo root.
	b, err := os.ReadFile("../../web/static/css/billing-invoices.css")
	if err != nil {
		t.Fatalf("read billing-invoices.css: %v", err)
	}
	return string(b)
}

func TestBillingInvoicesCSS_TokensOnly(t *testing.T) {
	t.Parallel()
	body := readBillingInvoicesCSS(t)
	// Strip comments: the header legitimately documents token names and may
	// reference identifiers; the contract applies to declarations only.
	decls := cssCommentRE.ReplaceAllString(body, "")

	if m := cssBareHexRE.FindAllString(decls, -1); len(m) > 0 {
		t.Errorf("billing-invoices.css must use design tokens for colour, found bare hex literal(s): %v", m)
	}
	if m := cssTokenFallbk.FindAllString(decls, -1); len(m) > 0 {
		t.Errorf("billing-invoices.css must not carry var(--token, fallback) — tokens.css is loaded first; found: %v", m)
	}
}

func TestBillingInvoicesCSS_StatusPillsUseAATextTokens(t *testing.T) {
	t.Parallel()
	body := readBillingInvoicesCSS(t)
	decls := cssCommentRE.ReplaceAllString(body, "")

	// The pale --color-*-surface backgrounds need the AA text-on-surface
	// tokens. --color-*-text is "text ON the fill" (white/near-black) and is
	// illegible on the pale surfaces — it must never style pill text here.
	for _, banned := range []string{"--color-warning-text", "--color-success-text"} {
		if strings.Contains(decls, banned) {
			t.Errorf("pill text must not use %q (text-on-fill token); use the AA text-on-surface --color-*-strong token instead", banned)
		}
	}
	for _, required := range []string{"--color-warning-strong", "--color-success-strong"} {
		if !strings.Contains(decls, required) {
			t.Errorf("expected AA text-on-surface token %q to style the status pills", required)
		}
	}
}
