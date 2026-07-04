package httpapi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TestRouter_InviteToken_NotLoggedInAccessLog is the SIN-66513 regression
// test. The invite token is an unauthenticated set-password credential
// (SIN-66510); the cross-cutting slogRequestLogger runs on the root
// router BEFORE the handler and used to log slog.String("path",
// r.URL.Path), leaking the plaintext token into the access log
// (CWE-532). The fix logs the matched chi route pattern instead. This
// test drives GET /invite/<token> through the real router with a
// captured JSON slog handler and asserts the emitted access-log record
// (a) never contains the plaintext token and (b) carries the redacted
// pattern "/invite/{token}". It fails against the raw-path logger and
// passes after the fix.
func TestRouter_InviteToken_NotLoggedInAccessLog(t *testing.T) {
	t.Parallel()

	const secretToken = "S3cr3t-256bit-invite-token-deadbeefdeadbeefdeadbeefdeadbeef"

	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}

	var buf bytes.Buffer
	logger := obs.NewJSONLogger(&buf, slog.LevelDebug)

	// WebInvite is public (no session): a trivial 200 handler is enough
	// to exercise the route + logger. The middleware chain (TenantScope,
	// request logger) is what we're testing, not the invite handler.
	invite := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(tenantIDs),
		TenantResolver: &fakeResolver{byHost: tenants},
		Logger:         logger,
		Metrics:        obs.NewMetrics(),
		WebInvite:      invite,
	})

	rec := do(t, h, http.MethodGet, "acme.crm.local", "/invite/"+secretToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (invite route should be mounted)", rec.Code)
	}

	// The whole log stream must never contain the plaintext token.
	if strings.Contains(buf.String(), secretToken) {
		t.Fatalf("access log leaked the plaintext invite token (CWE-532):\n%s", buf.String())
	}

	// The "http: request" access-log record must carry the redacted
	// route pattern in its "path" attribute.
	var found bool
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var logRec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &logRec); err != nil {
			continue
		}
		if logRec["msg"] != "http: request" {
			continue
		}
		found = true
		if got, _ := logRec["path"].(string); got != "/invite/{token}" {
			t.Fatalf("access-log path=%q, want redacted pattern %q", got, "/invite/{token}")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log buffer: %v", err)
	}
	if !found {
		t.Fatalf("no \"http: request\" access-log record emitted:\n%s", buf.String())
	}
}
