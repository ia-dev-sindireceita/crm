package main

import "testing"

// TestIAMRoutesIncludesDashboard pins the stdlib-mux delegation for the
// SIN-65008 managerial dashboard surface.
//
// router.go mounts GET /dashboard and GET /dashboard/export.csv inside the
// chi authed/tenanted group (guarded by deps.WebDashboard != nil), but those
// routes are only reachable if the public stdlib mux delegates the prefixes
// to the chi router. iamRoutes is that delegation list.
//
// SIN-65583 (parent SIN-65576): the prefixes were missing here, so every
// /dashboard* request fell through to the custom-domain catch-all at "/" and
// returned a raw 404 in staging even though dashboard_wire.go produced a
// fully-wired, non-nil handler (so the /hello-tenant "Painel / relatórios"
// tile rendered, then 404'd on click). Same defect class as the SIN-64975
// branding and SIN-64973 ai-policy mounts. This assertion fails without the
// fix and catches a regression that drops either prefix — the exact
// "/dashboard" (GET page) or the "/dashboard/" subtree (export.csv).
func TestIAMRoutesIncludesDashboard(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"/dashboard": false, "/dashboard/": false}
	for _, r := range iamRoutes {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("iamRoutes does not contain %q — the SIN-65008 dashboard mount would be unreachable (404 at the custom-domain catch-all)", route)
		}
	}
}
