package httpapi_test

// SIN-64977 — mount-point tests for the contacts MANAGEMENT routes
// (list/search + edit) added on top of the SIN-62855 identity-split
// mount. They pin two things the router branch promises:
//
//   - GET /contacts and the /{contactID}/edit verbs reach the inner
//     WebContacts handler with the iam.Principal attached (RequireAuth).
//   - With an Authorizer wired, GET /contacts gates on
//     ActionTenantContactRead and the edit verbs gate on
//     ActionTenantContactUpdate — the seed atendente role holds both,
//     while the common role is denied the write gate (least privilege).
//
// They reuse the recordingContacts handler + roled IAM harness from the
// sibling test files (same package) so no existing fixture is touched.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// --- ungated branch (Authorizer nil) ---------------------------------

func TestRouter_WebContacts_ListReachesHandler(t *testing.T) {
	t.Parallel()
	contacts := &recordingContacts{}
	h, _ := newWebContactsRouter(t, "tok-list", contacts, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	rec := do(t, h, http.MethodGet, host, "/contacts", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if len(contacts.calls) != 1 || contacts.calls[0].path != "/contacts" {
		t.Fatalf("inner calls=%+v want one GET /contacts", contacts.calls)
	}
	if !contacts.calls[0].hadPrincipal {
		t.Fatalf("list handler ran without iam.Principal (RequireAuth missing)")
	}
}

func TestRouter_WebContacts_EditReachesHandler(t *testing.T) {
	t.Parallel()
	contacts := &recordingContacts{}
	h, _ := newWebContactsRouter(t, "tok-edit", contacts, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	id := uuid.New().String()
	rec := do(t, h, http.MethodGet, host, "/contacts/"+id+"/edit", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if len(contacts.calls) != 1 || contacts.calls[0].path != "/contacts/"+id+"/edit" {
		t.Fatalf("inner calls=%+v want one GET /contacts/%s/edit", contacts.calls, id)
	}
}

// --- gated branch (Authorizer set) -----------------------------------

func mgmtAuthzRouter(t *testing.T, store httpapi.IAMService, resolver tenancy.Resolver, contacts http.Handler) (http.Handler, *authzRecorder) {
	t.Helper()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		Authorizer:     audited,
		WebContacts:    contacts,
	})
	return h, rec
}

func TestRouter_WebContacts_Authz_AtendenteAllowedOnListAndEdit(t *testing.T) {
	t.Parallel()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	store := newRoledIAM(map[string]uuid.UUID{host: acmeID})
	store.addUser(host, "att@acme.test", "pw-att", iam.RoleTenantAtendente, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	contacts := &recordingContacts{}
	h, _ := mgmtAuthzRouter(t, store, resolver, contacts)
	cookie := loginCookie(t, h, host, "att@acme.test", "pw-att")

	// read gate: atendente holds ActionTenantContactRead → 200
	if rec := do(t, h, http.MethodGet, host, "/contacts", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET /contacts status=%d want 200 (atendente read)", rec.Code)
	}
	// write gate: atendente holds ActionTenantContactUpdate → 200
	id := uuid.New().String()
	if rec := do(t, h, http.MethodGet, host, "/contacts/"+id+"/edit", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET /contacts/{id}/edit status=%d want 200 (atendente update)", rec.Code)
	}
	if len(contacts.calls) != 2 {
		t.Fatalf("inner call count=%d want 2 (list + edit reached handler)", len(contacts.calls))
	}
}

func TestRouter_WebContacts_Authz_CommonDeniedOnEdit(t *testing.T) {
	t.Parallel()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	store := newRoledIAM(map[string]uuid.UUID{host: acmeID})
	store.addUser(host, "com@acme.test", "pw-com", iam.RoleTenantCommon, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	contacts := &recordingContacts{}
	h, _ := mgmtAuthzRouter(t, store, resolver, contacts)
	cookie := loginCookie(t, h, host, "com@acme.test", "pw-com")

	// read gate: common holds ActionTenantContactRead → 200
	if rec := do(t, h, http.MethodGet, host, "/contacts", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET /contacts status=%d want 200 (common read)", rec.Code)
	}
	// write gate: common lacks ActionTenantContactUpdate → 403, handler not reached
	id := uuid.New().String()
	rec := do(t, h, http.MethodGet, host, "/contacts/"+id+"/edit", nil, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /contacts/{id}/edit status=%d want 403 (common denied update)", rec.Code)
	}
	for _, c := range contacts.calls {
		if c.path == "/contacts/"+id+"/edit" {
			t.Fatalf("edit handler reached despite RequireAction(update) deny")
		}
	}
}
