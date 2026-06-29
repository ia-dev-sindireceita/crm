package postgres_test

// SIN-66305 (R3 / SIN-66292) — real-pg coverage for the WhatsApp-session
// transition audit and the reserved system-principal hardening.
//
//   - The auditor writes a tamper-evident audit_log_security row from a
//     BACKGROUND (no-request) context: it pins app.tenant_id via WithTenant
//     so the app_runtime RLS WITH CHECK passes, reusing SplitAuditLogger for
//     the row. The payload carries NO phone (gate 5 / LGPD).
//   - The seeded system principal is hardened: it is excluded from the master
//     credential reader (gate 2) so MasterLogin fails closed (gate 1), it is
//     invisible to the master directory (gate 2), and the seeded row has the
//     reserved shape (gate 3/4: is_master, tenant_id NULL, is_system, sentinel
//     hash).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// freshDBWithSecurityAudit brings up a per-test DB with the users table, the
// reserved system principal (0126), the split-audit table (0083) and the
// wa_session event-type CHECK (0127) — the set the transition auditor needs.
func freshDBWithSecurityAudit(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0083_split_audit_log.up.sql",
		"0126_wa_session_transition_audit.up.sql",
		"0127_audit_log_security_wa_session.up.sql",
	} {
		body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	// The SplitAuditLogger INSERT writes correlation_id (added by 0117 with an
	// FK to master_impersonation_session — a deep chain irrelevant here: our
	// background transition audit never carries a correlation). Add just the
	// column so the canonical writer runs, without dragging the FK chain.
	if _, err := db.AdminPool().Exec(ctx,
		`ALTER TABLE audit_log_security ADD COLUMN IF NOT EXISTS correlation_id uuid`); err != nil {
		t.Fatalf("add correlation_id column: %v", err)
	}
	return db
}

func TestNewWASessionTransitionAuditor_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewWASessionTransitionAuditor(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

func TestWASessionTransitionAuditor_RecordsRow(t *testing.T) {
	db := freshDBWithSecurityAudit(t)
	ctx := context.Background()

	// app_runtime + zero actor must fail eagerly (real pool available here).
	if _, err := postgres.NewWASessionTransitionAuditor(db.RuntimePool(), uuid.Nil); !errors.Is(err, postgres.ErrZeroActor) {
		t.Fatalf("zero actor err = %v, want ErrZeroActor", err)
	}

	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "wa-audit", "wa-audit.crm.local"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	auditor, err := postgres.NewWASessionTransitionAuditor(db.RuntimePool(), iam.SystemPrincipalID())
	if err != nil {
		t.Fatalf("NewWASessionTransitionAuditor: %v", err)
	}

	if err := auditor.RecordTransition(ctx, uuid.Nil, audit.SecurityEventWASessionBanned, "connected", "banned", "x"); !errors.Is(err, postgres.ErrZeroTenant) {
		t.Fatalf("zero tenant err = %v, want ErrZeroTenant", err)
	}

	if err := auditor.RecordTransition(ctx, tenantID, audit.SecurityEventWASessionBanned, "connected", "banned", "logged out by whatsapp"); err != nil {
		t.Fatalf("RecordTransition(banned): %v", err)
	}

	// Read the row back via the admin pool (BYPASSRLS) and assert every
	// column plus the PII-free target payload.
	var (
		eventType string
		actorID   uuid.UUID
		gotTenant uuid.UUID
		targetRaw []byte
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT event_type, actor_user_id, tenant_id, target
		   FROM audit_log_security WHERE tenant_id = $1`, tenantID,
	).Scan(&eventType, &actorID, &gotTenant, &targetRaw); err != nil {
		t.Fatalf("read back audit row: %v", err)
	}
	if eventType != "wa_session.banned" {
		t.Errorf("event_type = %q, want wa_session.banned", eventType)
	}
	if actorID != iam.SystemPrincipalID() {
		t.Errorf("actor_user_id = %s, want system principal %s", actorID, iam.SystemPrincipalID())
	}
	if gotTenant != tenantID {
		t.Errorf("tenant_id = %s, want %s", gotTenant, tenantID)
	}
	var target map[string]any
	if err := json.Unmarshal(targetRaw, &target); err != nil {
		t.Fatalf("target jsonb unmarshal: %v", err)
	}
	if target["transport"] != "wa_session" || target["from"] != "connected" || target["to"] != "banned" || target["reason"] != "logged out by whatsapp" {
		t.Errorf("target = %v, want transport/from/to/reason set", target)
	}
	// Gate 5 (LGPD): no phone / MSISDN anywhere in the payload.
	for _, k := range []string{"phone", "msisdn", "sender", "e164", "number"} {
		if _, ok := target[k]; ok {
			t.Errorf("target leaked PII key %q: %v", k, target)
		}
	}

	// A disconnected transition writes its own distinct event type.
	other := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		other, "wa-audit2", "wa-audit2.crm.local"); err != nil {
		t.Fatalf("seed tenant2: %v", err)
	}
	if err := auditor.RecordTransition(ctx, other, audit.SecurityEventWASessionDisconnected, "connected", "disconnected", ""); err != nil {
		t.Fatalf("RecordTransition(disconnected): %v", err)
	}
	var got string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT event_type FROM audit_log_security WHERE tenant_id = $1`, other,
	).Scan(&got); err != nil {
		t.Fatalf("read disconnected row: %v", err)
	}
	if got != "wa_session.disconnected" {
		t.Errorf("event_type = %q, want wa_session.disconnected", got)
	}
}

// Gate 3/4: the seeded principal has exactly the reserved, hardened shape.
func TestSystemPrincipal_SeededAndHardened(t *testing.T) {
	db := freshDBWithMasterMFA(t) // applies 0126
	ctx := context.Background()

	var (
		tenantID *uuid.UUID
		email    string
		hash     string
		role     string
		isMaster bool
		isSystem bool
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT tenant_id, email, password_hash, role, is_master, is_system
		   FROM users WHERE id = $1`, iam.SystemPrincipalID(),
	).Scan(&tenantID, &email, &hash, &role, &isMaster, &isSystem); err != nil {
		t.Fatalf("read system principal: %v", err)
	}
	if tenantID != nil {
		t.Errorf("tenant_id = %v, want NULL (gate 4: never an impersonation target)", *tenantID)
	}
	if !isMaster || !isSystem {
		t.Errorf("is_master=%v is_system=%v, want both true", isMaster, isSystem)
	}
	if email != iam.SystemPrincipalEmail {
		t.Errorf("email = %q, want %q", email, iam.SystemPrincipalEmail)
	}
	if hash != iam.PasswordSentinelHash {
		t.Errorf("password_hash = %q, want the un-decodable sentinel %q", hash, iam.PasswordSentinelHash)
	}
	if role != "master" {
		t.Errorf("role = %q, want master", role)
	}
}

// Gate 1+2: the master credential reader excludes the system principal, so
// MasterLogin against its address fails closed with ErrInvalidCredentials.
func TestMasterLogin_SystemPrincipal_FailsClosed(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx := context.Background()
	actor := seedMasterUser(t, db, "ops-actor@sysprin.test")

	reader, err := postgres.NewMasterCredentialReader(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterCredentialReader: %v", err)
	}

	// Reader excludes it: zero-id sentinel, not a row.
	id, hash, err := reader.LookupMasterCredentials(ctx, iam.SystemPrincipalEmail)
	if err != nil {
		t.Fatalf("LookupMasterCredentials: %v", err)
	}
	if id != uuid.Nil || hash != "" {
		t.Fatalf("system principal resolved as a login candidate: id=%s hash=%q", id, hash)
	}

	// End-to-end: MasterLogin fails closed regardless of the password tried.
	svc := &iam.Service{MasterUsers: reader}
	for _, pw := range []string{iam.PasswordSentinelHash, "guess", ""} {
		if _, err := svc.MasterLogin(ctx, "host", iam.SystemPrincipalEmail, pw, nil, "", "/m/login"); !errors.Is(err, iam.ErrInvalidCredentials) {
			t.Fatalf("MasterLogin(system, %q) err = %v, want ErrInvalidCredentials", pw, err)
		}
	}
}

// Gate 2: the system principal is invisible to the master directory.
func TestMasterDirectory_ExcludesSystemPrincipal(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx := context.Background()
	actor := seedMasterUser(t, db, "ops-actor@sysdir.test")

	dir, err := postgres.NewMasterDirectory(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterDirectory: %v", err)
	}
	if _, err := dir.EmailFor(ctx, iam.SystemPrincipalID()); err == nil {
		t.Fatal("EmailFor(system principal) returned no error; it must be excluded (not found)")
	}
}
