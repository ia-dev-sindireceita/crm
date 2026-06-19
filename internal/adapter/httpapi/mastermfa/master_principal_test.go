package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
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
			mw := mastermfa.MasterHostOnly(tc.masterHost, nil)
			r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
			r.Host = tc.reqHost
			w := httptest.NewRecorder()
			mw(next).ServeHTTP(w, r)
			if tc.want404 {
				if w.Code != http.StatusNotFound || reached {
					t.Fatalf("want 404 + not reached: code=%d reached=%v", w.Code, reached)
				}
			} else {
				if !reached {
					t.Fatalf("want reached on matching host: code=%d", w.Code)
				}
			}
		})
	}
}
