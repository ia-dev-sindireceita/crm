package main

import "testing"

// TestIAMRoutesIncludesWASession pins the stdlib-mux delegation for the
// SIN-66259 / Fase 4 WhatsApp session provisioning surface.
//
// router.go mounts the five /settings/whatsapp-session* routes inside the chi
// authed/tenanted group (guarded by deps.WebWASession != nil), but they are
// only reachable if the public stdlib mux delegates the prefixes to chi.
// iamRoutes is that delegation list — the same chi-enumeration route-miss
// failure mode that bit /dashboard (SIN-65583), /branding (SIN-64975) and
// /ai-policy (SIN-64973). This assertion fails if either the exact
// "/settings/whatsapp-session" (page GET) or the "/settings/whatsapp-session/"
// subtree (status / consent / connect / disconnect) is dropped.
func TestIAMRoutesIncludesWASession(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"/settings/whatsapp-session":  false,
		"/settings/whatsapp-session/": false,
	}
	for _, r := range iamRoutes {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("iamRoutes does not contain %q — the SIN-66259 provisioning mount would 404 at the custom-domain catch-all", route)
		}
	}
}
