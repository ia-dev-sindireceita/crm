package main

import "testing"

// TestIAMRoutesIncludesBillingAndWallet pins the stdlib-mux delegation for the
// SIN-66550 gerente billing (PIX invoice) and SIN-63942 wallet surfaces.
//
// router.go mounts GET /billing/invoices[/{id}[/status]] + /billing/dunning-
// banner (guarded by RequireAction(ActionTenantBillingView)) and GET /wallet +
// /wallet/topup|ledger|ledger.csv (guarded by
// RequireAction(ActionTenantWalletViewLedger)) inside the chi authed/tenanted
// group. Those routes are only reachable if the public stdlib mux delegates the
// prefixes to the chi router — iamRoutes is that delegation list.
//
// SIN-66551 (parent SIN-66550): the prefixes were missing here, so every
// /billing* and /wallet* request fell through to the custom-domain catch-all at
// "/" and returned a raw 404 in staging even though the nav <a href> renders and
// the role gate is correct — clicking "Fatura" or "Wallet" 404'd. Same defect
// class as the SIN-64975 branding and SIN-65576 dashboard mounts. This assertion
// fails without the fix and catches a regression that drops any of the four
// prefixes — the exact "/wallet" (GET root) or the "/wallet/" subtree, and
// "/billing/invoices" (the invoice GETs) or the "/billing/" subtree
// (dunning-banner).
func TestIAMRoutesIncludesBillingAndWallet(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"/billing/invoices": false,
		"/billing/":         false,
		"/wallet":           false,
		"/wallet/":          false,
	}
	for _, r := range iamRoutes {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("iamRoutes does not contain %q — the SIN-66550 billing/wallet mount would be unreachable (404 at the custom-domain catch-all)", route)
		}
	}
}
