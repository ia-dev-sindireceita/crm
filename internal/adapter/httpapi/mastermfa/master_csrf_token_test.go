package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

func csrfTokReq(t *testing.T, m *mastermfa.Master) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
	if m != nil {
		r = r.WithContext(mastermfa.WithMaster(r.Context(), *m))
	}
	return r
}

func TestCSRFTokenFromContext_NoMaster_Empty(t *testing.T) {
	t.Parallel()
	// No master in context → empty so the masterweb handler fail-closes
	// (500), matching the tenant provider's programmer-error contract.
	if got := mastermfa.CSRFTokenFromContext(csrfTokReq(t, nil)); got != "" {
		t.Fatalf("want empty token without master, got %q", got)
	}
}

func TestCSRFTokenFromContext_NilUUID_Empty(t *testing.T) {
	t.Parallel()
	// A zero-value Master (ID == uuid.Nil) is treated as "no master" by
	// MasterFromContext, so the token must be empty (fail closed) even
	// though a Master value IS present in the context.
	if got := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{})); got != "" {
		t.Fatalf("want empty token for nil-uuid master, got %q", got)
	}
}

func TestCSRFTokenFromContext_WithMaster_NonEmptyHex(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	got := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{ID: id, Email: "ops@master.test"}))
	if got == "" {
		t.Fatal("want non-empty token with master in context, got empty")
	}
	// SHA-256 hex digest is 64 lowercase hex chars.
	if len(got) != 64 {
		t.Fatalf("want 64-char hex digest, got %d chars: %q", len(got), got)
	}
	if strings.ToLower(got) != got {
		t.Fatalf("want lowercase hex, got %q", got)
	}
	// Opaque: must NOT leak the raw operator UUID into the token.
	if strings.Contains(got, id.String()) {
		t.Fatalf("token %q leaks raw operator UUID %q", got, id)
	}
}

func TestCSRFTokenFromContext_DeterministicPerOperator(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	a := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{ID: id}))
	b := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{ID: id}))
	if a != b {
		t.Fatalf("token not stable per operator: %q != %q", a, b)
	}
}

func TestCSRFTokenFromContext_DistinctPerOperator(t *testing.T) {
	t.Parallel()
	a := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{ID: uuid.New()}))
	b := mastermfa.CSRFTokenFromContext(csrfTokReq(t, &mastermfa.Master{ID: uuid.New()}))
	if a == b {
		t.Fatalf("distinct operators produced the same token %q", a)
	}
}
