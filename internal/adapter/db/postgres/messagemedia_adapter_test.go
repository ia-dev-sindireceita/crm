package postgres_test

// SIN-62804 / F2-05c integration tests for the messagemedia Postgres
// adapter. Hosted in the parent postgres_test package for the same
// shared-TestMain reason as inbox_adapter_test.go and
// contacts_adapter_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	pgmessagemedia "github.com/pericles-luz/crm/internal/adapter/db/postgres/messagemedia"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/media/scanner"
)

// freshDBForMessageMedia applies the inbox-contacts stack and then the
// 0094_message_media_scan_status migration that adds the jsonb column
// the adapter writes to.
func freshDBForMessageMedia(t *testing.T) *testpg.DB {
	t.Helper()
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(harness.MigrationsDir(), "0094_message_media_scan_status.up.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read 0094: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0094: %v", err)
	}
	return db
}

// seedMessageWithMedia creates a contact + conversation + message and
// patches the message's `media` column with the supplied JSON so each
// test starts from a known scan_status state.
func seedMessageWithMedia(t *testing.T, db *testpg.DB, mediaJSON string) (uuid.UUID, *inbox.Message) {
	t.Helper()
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	conv := inbox.HydrateConversation(uuid.New(), tenant, contact.ID, "whatsapp",
		inbox.ConversationStateOpen, nil, time.Time{}, time.Time{})
	inboxStore := newInboxStore(t, db)
	if err := inboxStore.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	msg := inbox.HydrateMessage(uuid.New(), tenant, conv.ID, inbox.MessageDirectionIn,
		"hi", inbox.MessageStatusDelivered, "", nil, time.Time{})
	if err := inboxStore.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE message SET media = $1::jsonb WHERE id = $2`, mediaJSON, msg.ID); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	return tenant, msg
}

func readMediaJSON(t *testing.T, db *testpg.DB, msgID uuid.UUID) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT media FROM message WHERE id = $1`, msgID).Scan(&raw); err != nil {
		t.Fatalf("read media: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------

func TestMessageMediaAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgmessagemedia.New(nil); err == nil {
		t.Error("New(nil) err = nil, want ErrNilPool")
	}
}

func TestMessageMediaAdapter_UpdateScanResult_RejectsZeroIDs(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.UpdateScanResult(context.Background(), uuid.Nil, uuid.New(),
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}); err == nil {
		t.Error("expected error for zero tenant")
	}
	err = store.UpdateScanResult(context.Background(), uuid.New(), uuid.Nil,
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"})
	if !errors.Is(err, scanner.ErrNotFound) {
		t.Errorf("zero message id err = %v, want ErrNotFound", err)
	}
}

func TestMessageMediaAdapter_UpdateScanResult_RejectsNonTerminalStatus(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bad := []scanner.Status{scanner.StatusPending, scanner.Status("garbage")}
	for _, st := range bad {
		err := store.UpdateScanResult(context.Background(), uuid.New(), uuid.New(),
			scanner.ScanResult{Status: st})
		if err == nil {
			t.Errorf("expected error for non-terminal status %q", st)
		}
	}
}

func TestMessageMediaAdapter_UpdateScanResult_HappyPath_Clean(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant, msg := seedMessageWithMedia(t, db,
		`{"key":"media/x","scan_status":"pending"}`)

	err = store.UpdateScanResult(context.Background(), tenant, msg.ID,
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "clamav-1.2.3"})
	if err != nil {
		t.Fatalf("UpdateScanResult: %v", err)
	}

	got := readMediaJSON(t, db, msg.ID)
	if got["scan_status"] != "clean" || got["scan_engine"] != "clamav-1.2.3" {
		t.Errorf("media after update = %+v", got)
	}
	// Verify the existing "key" field is preserved (jsonb merge).
	if got["key"] != "media/x" {
		t.Errorf("expected key preserved, got %+v", got)
	}
}

func TestMessageMediaAdapter_UpdateScanResult_Infected(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant, msg := seedMessageWithMedia(t, db, `{"scan_status":"pending"}`)
	err = store.UpdateScanResult(context.Background(), tenant, msg.ID,
		scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "clamav-1.2.3"})
	if err != nil {
		t.Fatalf("UpdateScanResult: %v", err)
	}
	got := readMediaJSON(t, db, msg.ID)
	if got["scan_status"] != "infected" {
		t.Errorf("scan_status = %v, want infected", got["scan_status"])
	}
}

func TestMessageMediaAdapter_UpdateScanResult_NullMedia_Initialises(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Seed a message with media = NULL (text-only message that
	// somehow got scheduled for scan — exercises the COALESCE path).
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	conv := inbox.HydrateConversation(uuid.New(), tenant, contact.ID, "whatsapp",
		inbox.ConversationStateOpen, nil, time.Time{}, time.Time{})
	inboxStore := newInboxStore(t, db)
	if err := inboxStore.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msg := inbox.HydrateMessage(uuid.New(), tenant, conv.ID, inbox.MessageDirectionIn,
		"hi", inbox.MessageStatusDelivered, "", nil, time.Time{})
	if err := inboxStore.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	// `media` is NULL; the adapter's COALESCE(..., '{}'::jsonb) path
	// MUST allow the verdict to land because the WHERE clause treats
	// missing scan_status as pending.
	if err := store.UpdateScanResult(context.Background(), tenant, msg.ID,
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}); err != nil {
		t.Fatalf("UpdateScanResult on NULL media: %v", err)
	}
	got := readMediaJSON(t, db, msg.ID)
	if got["scan_status"] != "clean" {
		t.Errorf("scan_status = %v, want clean", got["scan_status"])
	}
}

func TestMessageMediaAdapter_UpdateScanResult_Idempotent_AlreadyFinalised(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"clean", "infected"} {
		terminal := terminal
		t.Run(terminal, func(t *testing.T) {
			// Fresh DB per sub-test so the seed helpers (which use
			// a hardcoded phone) do not collide on the contacts
			// uniqueness constraint.
			db := freshDBForMessageMedia(t)
			store, err := pgmessagemedia.New(db.RuntimePool())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tenant, msg := seedMessageWithMedia(t, db,
				fmt.Sprintf(`{"scan_status":%q,"scan_engine":"clamav-old"}`, terminal))
			err = store.UpdateScanResult(context.Background(), tenant, msg.ID,
				scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "clamav-new"})
			if !errors.Is(err, scanner.ErrAlreadyFinalised) {
				t.Fatalf("redelivery err = %v, want ErrAlreadyFinalised", err)
			}
			// Verify the row was not overwritten.
			got := readMediaJSON(t, db, msg.ID)
			if got["scan_engine"] != "clamav-old" {
				t.Errorf("scan_engine clobbered: %v", got)
			}
			if got["scan_status"] != terminal {
				t.Errorf("scan_status clobbered: %v", got)
			}
		})
	}
}

func TestMessageMediaAdapter_UpdateScanResult_MissingRow_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant := seedContactsTenant(t, db)
	missing := uuid.New()
	err = store.UpdateScanResult(context.Background(), tenant, missing,
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"})
	if !errors.Is(err, scanner.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMessageMediaAdapter_UpdateScanResult_CrossTenant_RLSHides(t *testing.T) {
	t.Parallel()
	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	otherTenant, msg := seedMessageWithMedia(t, db, `{"scan_status":"pending"}`)
	rogue := seedContactsTenant(t, db)
	// rogue is a different tenant; RLS hides the row, the adapter
	// surfaces ErrNotFound.
	err = store.UpdateScanResult(context.Background(), rogue, msg.ID,
		scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"})
	if !errors.Is(err, scanner.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
	// And the row owned by otherTenant was untouched.
	got := readMediaJSON(t, db, msg.ID)
	if got["scan_status"] != "pending" {
		t.Errorf("scan_status changed under cross-tenant write: %v", got)
	}
	_ = otherTenant
}
