package mastermfa_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
)

const testMasterHost = "master.crm.local"

// capturePrincipal is a downstream handler that records the principal it
// observed (if any) and whether it was reached.
type capturePrincipal struct {
	reached bool
	p       iam.Principal
	hadP    bool
}

func (c *capturePrincipal) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.reached = true
	c.p, c.hadP = iam.PrincipalFromContext(r.Context())
}

func masterReq(host, path string, withMaster bool) (*http.Request, uuid.UUID) {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	uid := uuid.New()
	if withMaster {
		r = r.WithContext(mastermfa.WithMaster(r.Context(),
			mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	}
	return r, uid
}

func TestRequirePrincipalFromMaster_OnHost_SynthesizesRoleMaster(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
	d := &capturePrincipal{}
	r, uid := masterReq(testMasterHost, "/master/tenants", true)
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)

	if !d.reached {
		t.Fatalf("downstream not reached on master host with valid master")
	}
	if !d.hadP {
		t.Fatalf("no principal synthesized")
	}
	if d.p.UserID != uid {
		t.Errorf("principal UserID: got %v want %v", d.p.UserID, uid)
	}
	if !d.p.IsMaster() {
		t.Errorf("principal is not RoleMaster: %+v", d.p.Roles)
	}
	if d.p.TenantID != uuid.Nil {
		t.Errorf("principal carries a TenantID (%v) — must be zero (C4)", d.p.TenantID)
	}
	if d.p.MasterImpersonating {
		t.Errorf("principal MasterImpersonating must be false on the direct operator surface")
	}
}

// SIN-65321 — alongside the principal, RequirePrincipalFromMaster must also
// seed an iam.Session via middleware.WithSession so the downstream
// ImpersonationFromSession middleware does not 503 "impersonation requires
// session" for an authenticated master operator. The synthesized session
// carries UserID=master.ID, Role=RoleMaster, TenantID=Nil and a zero ID/expiry
// (it is NOT a persisted session — the impersonation envelope keys off the
// master cookie, not iam.Session.ID).
func TestRequirePrincipalFromMaster_OnHost_SynthesizesSession(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
	var (
		reached bool
		sess    iam.Session
		hadSess bool
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		sess, hadSess = middleware.SessionFromContext(r.Context())
	})
	r, uid := masterReq(testMasterHost, "/master/tenants", true)
	w := httptest.NewRecorder()
	mw(next).ServeHTTP(w, r)

	if !reached {
		t.Fatalf("downstream not reached on master host with valid master")
	}
	if !hadSess {
		t.Fatalf("no session synthesized — ImpersonationFromSession would 503")
	}
	if sess.UserID != uid {
		t.Errorf("session UserID: got %v want %v", sess.UserID, uid)
	}
	if sess.Role != iam.RoleMaster {
		t.Errorf("session Role: got %q want %q", sess.Role, iam.RoleMaster)
	}
	if sess.TenantID != uuid.Nil {
		t.Errorf("session carries a TenantID (%v) — must be zero (C4, no tenant-scope leak)", sess.TenantID)
	}
	if sess.ID != uuid.Nil {
		t.Errorf("session ID must be zero (not a persisted session), got %v", sess.ID)
	}
	if !sess.ExpiresAt.IsZero() {
		t.Errorf("session ExpiresAt must be zero (not a persisted session), got %v", sess.ExpiresAt)
	}
}

// SIN-65321 — fail-closed paths must NOT seed a session. Off-host (host-pin
// 404) and master-miss (401) both short-circuit before synthesis, so no
// iam.Session may leak onto the context.
func TestRequirePrincipalFromMaster_FailClosed_NoSession(t *testing.T) {
	cases := []struct {
		name       string
		reqHost    string
		withMaster bool
		wantCode   int
	}{
		{"off host", "acme.crm.local", true, http.StatusNotFound},
		{"no master", testMasterHost, false, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
			reached := false
			hadSess := false
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reached = true
				_, hadSess = middleware.SessionFromContext(r.Context())
			})
			r, _ := masterReq(tc.reqHost, "/master/tenants", tc.withMaster)
			w := httptest.NewRecorder()
			mw(next).ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("status: got %d want %d", w.Code, tc.wantCode)
			}
			if reached {
				t.Fatalf("handler reached on fail-closed path")
			}
			if hadSess {
				t.Fatalf("session leaked onto context on fail-closed path")
			}
		})
	}
}

// SecEng C2 — the host pin. A valid master session on a TENANT host MUST
// NOT reach the handler and MUST NOT synthesize a RoleMaster principal.
func TestRequirePrincipalFromMaster_OffHost_404_NoSynthesis(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
	d := &capturePrincipal{}
	r, _ := masterReq("acme.crm.local", "/master/tenants", true)
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("off-host status: got %d want 404", w.Code)
	}
	if d.reached {
		t.Fatalf("handler reached off-host — host pin breached (C2)")
	}
}

// Empty MasterHost disables synthesis entirely (fail closed) — must not
// fall back to "match any host".
func TestRequirePrincipalFromMaster_EmptyHost_FailsClosed(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: ""})
	d := &capturePrincipal{}
	r, _ := masterReq("master.crm.local", "/master/tenants", true)
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || d.reached {
		t.Fatalf("empty MasterHost must fail closed: code=%d reached=%v", w.Code, d.reached)
	}
}

// Host comparison is normalized: a ":port" suffix and casing must not
// defeat the pin.
func TestRequirePrincipalFromMaster_HostNormalization(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
	d := &capturePrincipal{}
	r, _ := masterReq("Master.CRM.local:8080", "/master/tenants", true)
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if !d.reached || !d.hadP {
		t.Fatalf("normalized host (port+case) failed to match: code=%d reached=%v", w.Code, d.reached)
	}
}

// C1 — fail closed when the master context is absent (auth/MFA chain did
// not run). Must NOT synthesize a zero-UUID principal.
func TestRequirePrincipalFromMaster_NoMaster_401_NoSynthesis(t *testing.T) {
	mw := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: testMasterHost})
	d := &capturePrincipal{}
	r, _ := masterReq(testMasterHost, "/master/tenants", false)
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if d.reached {
		t.Fatalf("handler reached without a master in context")
	}
}

// --- MasterHostOnly outer gate ---

// recordingDeniedAuditor captures master-access-denied emissions for the
// SIN-65269 R2 (re-homed CA #2 deny-audit) regression assertions.
type recordingDeniedAuditor struct {
	reasons []string
	paths   []string
	hosts   []string
}

func (a *recordingDeniedAuditor) LogMasterAccessDenied(_ context.Context, reason, path, host string) error {
	a.reasons = append(a.reasons, reason)
	a.paths = append(a.paths, path)
	a.hosts = append(a.hosts, host)
	return nil
}

func TestMasterHostOnly(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	cases := []struct {
		name       string
		masterHost string
		reqHost    string
		want404    bool
	}{
		{"on host", testMasterHost, testMasterHost, false},
		{"on host with port", testMasterHost, testMasterHost + ":8443", false},
		{"off host", testMasterHost, "acme.crm.local", true},
		{"empty config fails closed", "", testMasterHost, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			auditor := &recordingDeniedAuditor{}
			mw := mastermfa.MasterHostOnly(tc.masterHost, nil, auditor)
			r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
			r.Host = tc.reqHost
			w := httptest.NewRecorder()
			mw(next).ServeHTTP(w, r)
			if tc.want404 {
				if w.Code != http.StatusNotFound || reached {
					t.Fatalf("want 404 + not reached: code=%d reached=%v", w.Code, reached)
				}
				// R2: the host-pin 404 path MUST emit one off_host
				// deny-audit carrying the path + source host (never a cookie).
				if len(auditor.reasons) != 1 || auditor.reasons[0] != mastermfa.MasterDeniedReasonOffHost {
					t.Fatalf("want one off_host deny-audit, got %v", auditor.reasons)
				}
				if auditor.paths[0] != "/master/tenants" || auditor.hosts[0] != tc.reqHost {
					t.Fatalf("audit row payload = path %q host %q, want /master/tenants + %q", auditor.paths[0], auditor.hosts[0], tc.reqHost)
				}
			} else {
				if !reached {
					t.Fatalf("want reached on matching host: code=%d", w.Code)
				}
				if len(auditor.reasons) != 0 {
					t.Fatalf("on-host request must not emit a deny-audit, got %v", auditor.reasons)
				}
			}
		})
	}
}

// TestMasterHostOnly_NilAuditor is the back-compat guard: a nil auditor
// must be a no-op, not a panic.
func TestMasterHostOnly_NilAuditor(t *testing.T) {
	mw := mastermfa.MasterHostOnly(testMasterHost, nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
	r.Host = "acme.crm.local"
	w := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}
