package lgpd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/lgpd"
)

func TestWriteBundle_RejectsNilWriter(t *testing.T) {
	if err := lgpd.WriteBundle(nil, lgpd.ExportBundle{}); err == nil {
		t.Fatal("WriteBundle(nil) err = nil, want non-nil")
	}
}

func TestWriteBundle_ContainsBothFiles(t *testing.T) {
	contactID := uuid.New()
	tenantID := uuid.New()
	convID := uuid.New()
	msgID := uuid.New()
	bundle := lgpd.ExportBundle{
		GeneratedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Contact: lgpd.ExportContact{
			ID:          contactID,
			TenantID:    tenantID,
			DisplayName: "Maria da Silva",
			CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Identities: []lgpd.ExportIdentity{
			{ID: uuid.New(), Channel: "whatsapp", ExternalID: "+5511999999999", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		Conversations: []lgpd.ExportConversation{
			{ID: convID, Channel: "whatsapp", State: "open", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		Messages: []lgpd.ExportMessage{
			{ID: msgID, ConversationID: convID, Direction: "in", Body: "olá", Status: "delivered", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
	}

	var buf bytes.Buffer
	if err := lgpd.WriteBundle(&buf, bundle); err != nil {
		t.Fatalf("WriteBundle err = %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader err = %v", err)
	}
	wantFiles := map[string]bool{"data.json": false, "data.csv": false}
	for _, f := range zr.File {
		wantFiles[f.Name] = true
	}
	for name, ok := range wantFiles {
		if !ok {
			t.Errorf("zip missing %q", name)
		}
	}

	// JSON round-trips into the same shape.
	jsonF, err := openZipEntry(zr, "data.json")
	if err != nil {
		t.Fatalf("open data.json err = %v", err)
	}
	var decoded lgpd.ExportBundle
	if err := json.NewDecoder(jsonF).Decode(&decoded); err != nil {
		t.Fatalf("decode data.json err = %v", err)
	}
	if decoded.Contact.ID != contactID {
		t.Errorf("decoded.Contact.ID = %s, want %s", decoded.Contact.ID, contactID)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Body != "olá" {
		t.Errorf("decoded messages = %+v", decoded.Messages)
	}

	// CSV has a section per slice.
	csvF, err := openZipEntry(zr, "data.csv")
	if err != nil {
		t.Fatalf("open data.csv err = %v", err)
	}
	body, err := io.ReadAll(csvF)
	if err != nil {
		t.Fatalf("read csv err = %v", err)
	}
	got := string(body)
	for _, banner := range []string{
		"# contact",
		"# identities",
		"# conversations",
		"# messages",
		"# billing_events",
		"# consents",
	} {
		if !strings.Contains(got, banner) {
			t.Errorf("csv missing banner %q\nfull body:\n%s", banner, got)
		}
	}
	if !strings.Contains(got, "Maria da Silva") {
		t.Error("csv missing display_name 'Maria da Silva'")
	}
}

func TestWriteBundle_FullPayloadRoundtrip(t *testing.T) {
	contactID := uuid.New()
	tenantID := uuid.New()
	convID := uuid.New()
	last := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	ext := "wamid.xyz"
	mediaJSON := `{"kind":"image","mime":"image/png"}`
	bundle := lgpd.ExportBundle{
		GeneratedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Contact: lgpd.ExportContact{
			ID: contactID, TenantID: tenantID, DisplayName: "X",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Identities: []lgpd.ExportIdentity{
			{ID: uuid.New(), Channel: "sms", ExternalID: "+5511", CreatedAt: last},
		},
		Conversations: []lgpd.ExportConversation{
			{ID: convID, Channel: "wa", State: "closed", LastMessageAt: &last, CreatedAt: last},
		},
		Messages: []lgpd.ExportMessage{
			{ID: uuid.New(), ConversationID: convID, Direction: "out", Body: "b", Status: "delivered",
				ChannelExternalID: &ext, Media: &mediaJSON, CreatedAt: last},
		},
		BillingEvents: []lgpd.ExportBillingEvent{
			{ID: uuid.New(), EventType: "master.grant.issued", Target: `{"contact_id":"x"}`, OccurredAt: last},
		},
		Consents: []lgpd.ExportConsent{
			{ID: uuid.New(), ScopeKind: "tenant", ScopeID: "x", AnonymizerVersion: "1", PromptVersion: "2", AcceptedAt: last},
		},
	}
	var buf bytes.Buffer
	if err := lgpd.WriteBundle(&buf, bundle); err != nil {
		t.Fatalf("WriteBundle err = %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	csvF, _ := openZipEntry(zr, "data.csv")
	body, _ := io.ReadAll(csvF)
	got := string(body)
	for _, want := range []string{"wamid.xyz", "+5511", "master.grant.issued", "anonymizer", "image/png"} {
		if !strings.Contains(got, want) {
			t.Errorf("csv missing %q\n%s", want, got)
		}
	}
}

// errWriter fails every Write to exercise WriteBundle's error path.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, io.ErrShortWrite }

func TestWriteBundle_PropagatesWriterError(t *testing.T) {
	err := lgpd.WriteBundle(errWriter{}, lgpd.ExportBundle{})
	if err == nil {
		t.Fatal("WriteBundle(errWriter) err = nil, want non-nil")
	}
}

func openZipEntry(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, errFileNotFound
}

var errFileNotFound = &zipMissing{}

type zipMissing struct{}

func (e *zipMissing) Error() string { return "zip entry not found" }
