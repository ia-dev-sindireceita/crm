package postgres_test

// SIN-64972 integration tests for the webchat OriginValidator (ADR-0021
// D2 allowlist + D4 origin signature) against a real Postgres.

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func setWebchatOrigins(t *testing.T, db *testpg.DB, tenantID uuid.UUID, originsJSON string, secret []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE tenants SET webchat_allowed_origins = $2::jsonb, webchat_origin_secret = $3 WHERE id = $1`,
		tenantID, originsJSON, secret); err != nil {
		t.Fatalf("update tenant webchat origins: %v", err)
	}
}

func TestWebchatOrigins_Valid_AllowlistGate(t *testing.T) {
	db := freshDBWithWebchat(t)
	tenantID := seedWebchatTenant(t, db, "acme.crm.local")
	v := postgres.NewWebchatOrigins(db.RuntimePool())
	ctx := context.Background()

	// Empty allowlist (default '[]') is fail-closed.
	if ok, err := v.Valid(ctx, tenantID, "https://acme.com.br"); err != nil || ok {
		t.Fatalf("empty allowlist Valid = (%v, %v), want (false, nil)", ok, err)
	}

	setWebchatOrigins(t, db, tenantID, `["https://acme.com.br","https://www.acme.com.br"]`, []byte("secret"))

	cases := []struct {
		origin string
		want   bool
	}{
		{"https://acme.com.br", true},
		{"https://www.acme.com.br", true},
		{"https://acme.com.br:443", true},   // default port canonicalises away
		{"https://ACME.com.br", true},       // host case-insensitive
		{"https://evil.com", false},         // not allow-listed
		{"http://acme.com.br", false},       // scheme mismatch
		{"https://acme.com.br:8443", false}, // explicit non-default port differs
		{"not a url", false},                // malformed
	}
	for _, tc := range cases {
		ok, err := v.Valid(ctx, tenantID, tc.origin)
		if err != nil {
			t.Errorf("Valid(%q) unexpected err: %v", tc.origin, err)
			continue
		}
		if ok != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.origin, ok, tc.want)
		}
	}
}

func TestWebchatOrigins_HMAC(t *testing.T) {
	db := freshDBWithWebchat(t)
	tenantID := seedWebchatTenant(t, db, "acme.crm.local")
	v := postgres.NewWebchatOrigins(db.RuntimePool())
	ctx := context.Background()

	// No secret provisioned → fail-closed (error).
	if _, err := v.HMAC(ctx, tenantID, "https://acme.com.br"); err == nil {
		t.Fatalf("HMAC without secret must error (fail-closed)")
	}

	setWebchatOrigins(t, db, tenantID, `["https://acme.com.br"]`, []byte("topsecret"))

	sig, err := v.HMAC(ctx, tenantID, "https://acme.com.br:443")
	if err != nil {
		t.Fatalf("HMAC: %v", err)
	}
	if _, err := hex.DecodeString(sig); err != nil || len(sig) != 64 {
		t.Fatalf("HMAC sig = %q, want 64 hex chars: %v", sig, err)
	}
	// Canonicalisation: the :443 default port must produce the same
	// signature as the bare origin (D4 canonical_origin).
	sig2, err := v.HMAC(ctx, tenantID, "https://acme.com.br")
	if err != nil {
		t.Fatalf("HMAC bare: %v", err)
	}
	if sig != sig2 {
		t.Fatalf("HMAC not canonical: %q != %q", sig, sig2)
	}
}
