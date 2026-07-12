package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// TestChannelAssociationWriter_UpsertSQLAndArgs pins the exact query shape:
// an INSERT with the ON CONFLICT (channel, association) upsert clause, and the
// (tenantID, channel, association) argument order the schema expects. A real
// INSERT → Resolve round-trip against Postgres lives in the db/postgres
// package (channel_association_writer_roundtrip_test.go).
func TestChannelAssociationWriter_UpsertSQLAndArgs(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	var gotSQL string
	var gotArgs []any
	conn := stubConn{
		exec: func(sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return pgconn.CommandTag{}, nil
		},
	}
	err := pgstore.NewChannelAssociationWriter(conn).
		SaveAssociation(context.Background(), tenantID, "whatsapp", "5511999990000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotSQL, "INSERT INTO tenant_channel_associations") {
		t.Errorf("SQL missing INSERT target:\n%s", gotSQL)
	}
	if !strings.Contains(gotSQL, "ON CONFLICT (channel, association) DO UPDATE SET tenant_id = EXCLUDED.tenant_id") {
		t.Errorf("SQL missing upsert clause:\n%s", gotSQL)
	}
	if len(gotArgs) != 3 {
		t.Fatalf("want 3 args, got %d: %v", len(gotArgs), gotArgs)
	}
	if gotArgs[0] != tenantID {
		t.Errorf("arg0 tenantID=%v, want %v", gotArgs[0], tenantID)
	}
	if gotArgs[1] != "whatsapp" {
		t.Errorf("arg1 channel=%v, want whatsapp", gotArgs[1])
	}
	if gotArgs[2] != "5511999990000" {
		t.Errorf("arg2 association=%v, want 5511999990000", gotArgs[2])
	}
}

// TestChannelAssociationWriter_ErrorWrapped: a driver error is wrapped so
// callers can still errors.Is the original.
func TestChannelAssociationWriter_ErrorWrapped(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk full")
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, boom
		},
	}
	err := pgstore.NewChannelAssociationWriter(conn).
		SaveAssociation(context.Background(), uuid.New(), "whatsapp", "x")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped original error, got %v", err)
	}
}
