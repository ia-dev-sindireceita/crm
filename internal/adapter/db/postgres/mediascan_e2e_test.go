package postgres_test

// SIN-62804 / F2-05c end-to-end pipeline test:
//
//	publish media.scan.requested
//	  → mediascan-worker.Handle
//	    → ClamAV adapter (in-process clamd stub responding INFECTED to EICAR)
//	    → messagemedia Postgres adapter (real testpg cluster)
//	  → publish media.scan.completed
//
// Lives in the parent postgres_test package because the notenant
// analyzer (SIN-62232 / ADR 0071) blocks direct pgxpool calls outside
// the postgres adapter allowlist, and the test seeds rows directly
// via db.AdminPool() to bypass the inbox use case for speed. NATS
// comes from an embedded nats-server (no Docker required); the
// adapter under test still uses the real nats.go SDK against it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"

	pgmessagemedia "github.com/pericles-luz/crm/internal/adapter/db/postgres/messagemedia"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	clamavadapter "github.com/pericles-luz/crm/internal/adapter/media/clamav"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/media/scanner"
	"github.com/pericles-luz/crm/internal/media/worker"
)

// ---------------------------------------------------------------------
// embedded NATS
// ---------------------------------------------------------------------

func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	return s.ClientURL()
}

// ---------------------------------------------------------------------
// clamd stub
// ---------------------------------------------------------------------

const eicarMagic = "X5O!P%@AP[4\\PZX54(P^)"

// eicarClamd is a DialFunc that answers VERSION + INSTREAM. The
// INSTREAM verdict is INFECTED when the streamed body contains the
// EICAR magic; OK otherwise. One dial per command.
func eicarClamd() clamavadapter.DialFunc {
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			br := bufio.NewReader(server)
			cmd, err := br.ReadString(0)
			if err != nil {
				return
			}
			switch {
			case strings.Contains(cmd, "VERSION"):
				_, _ = server.Write([]byte("ClamAV 1.2.3/27000/test\x00"))
			case strings.Contains(cmd, "INSTREAM"):
				var body bytes.Buffer
				for {
					var sz uint32
					if err := binary.Read(br, binary.BigEndian, &sz); err != nil {
						return
					}
					if sz == 0 {
						break
					}
					chunk := make([]byte, sz)
					if _, err := io.ReadFull(br, chunk); err != nil {
						return
					}
					body.Write(chunk)
				}
				if bytes.Contains(body.Bytes(), []byte(eicarMagic)) {
					_, _ = server.Write([]byte("stream: Eicar-Signature FOUND\x00"))
				} else {
					_, _ = server.Write([]byte("stream: OK\x00"))
				}
			}
		}()
		return client, nil
	}
}

// ---------------------------------------------------------------------

type mapBlobs struct{ m map[string][]byte }

func (b *mapBlobs) Open(_ context.Context, key string) (io.ReadCloser, error) {
	v, ok := b.m[key]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", key)
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

// seedMediaScanMessage builds a tenant + contact + conversation +
// message row with `media = {scan_status:"pending"}` and returns the
// tuple needed to address it from outside. Reuses the existing inbox
// adapter rather than poking SQL, so a future schema change tripping
// the constructor fails the test loudly.
func seedMediaScanMessage(t *testing.T, db *testpg.DB) (tenant, msgID uuid.UUID, key string) {
	t.Helper()
	tenant, msgID = mustSeedTenantMessage(t, db)
	key = "media/" + tenant.String() + "/" + msgID.String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE message SET media = $1::jsonb WHERE id = $2`,
		fmt.Sprintf(`{"key":%q,"scan_status":"pending"}`, key), msgID); err != nil {
		t.Fatalf("set media pending: %v", err)
	}
	return tenant, msgID, key
}

func mustSeedTenantMessage(t *testing.T, db *testpg.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	store := newInboxStore(t, db)
	conv := inbox.HydrateConversation(uuid.New(), tenant, contact.ID, "whatsapp",
		inbox.ConversationStateOpen, nil, time.Time{}, time.Time{})
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msg := inbox.HydrateMessage(uuid.New(), tenant, conv.ID, inbox.MessageDirectionIn,
		"img", inbox.MessageStatusDelivered, "", nil, time.Time{})
	if err := store.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	return tenant, msg.ID
}

func readMediaScanStatusForE2E(t *testing.T, db *testpg.DB, msgID uuid.UUID) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT media FROM message WHERE id = $1`, msgID).Scan(&raw); err != nil {
		t.Fatalf("read media: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	st, _ := got["scan_status"].(string)
	en, _ := got["scan_engine"].(string)
	return st, en
}

// publisherShim adapts SDKAdapter to worker.Publisher for the test.
type publisherShim struct {
	a    *natsadapter.SDKAdapter
	seen atomic.Int32
}

func (p *publisherShim) Publish(ctx context.Context, subject string, body []byte) error {
	p.seen.Add(1)
	return p.a.Publish(ctx, subject, body)
}

// ---------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------

func TestMediaScan_E2E_InfectedEICAR(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test (Postgres + embedded NATS); skipped under -short")
	}
	t.Parallel()

	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("messagemedia.New: %v", err)
	}

	tenant, msgID, key := seedMediaScanMessage(t, db)
	blobs := &mapBlobs{m: map[string][]byte{key: []byte(eicarMagic + "...")}}

	scn, err := clamavadapter.New(clamavadapter.Config{Addr: "ignored", Dial: eicarClamd()}, blobs)
	if err != nil {
		t.Fatalf("clamav.New: %v", err)
	}

	url := startEmbeddedNATS(t)
	a, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL: url, ConnectTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(a.Close)
	if err := a.EnsureStream("MEDIA_E2E_INF", []string{worker.SubjectRequested, worker.SubjectCompleted}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	pub := &publisherShim{a: a}
	h, err := worker.New(scn, store, pub, nil)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	completedCh := make(chan worker.Completed, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = a.Subscribe(ctx, worker.SubjectCompleted, "g-c-inf", "d-c-inf", time.Second,
		func(_ context.Context, d *natsadapter.Delivery) error {
			var c worker.Completed
			_ = json.Unmarshal(d.Data(), &c)
			_ = d.Ack(context.Background())
			select {
			case completedCh <- c:
			default:
			}
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe completed: %v", err)
	}
	_, err = a.Subscribe(ctx, worker.SubjectRequested, "g-w-inf", "d-w-inf", 5*time.Second,
		func(ctx context.Context, d *natsadapter.Delivery) error {
			return h.Handle(ctx, d)
		})
	if err != nil {
		t.Fatalf("subscribe requested: %v", err)
	}

	reqBody, _ := json.Marshal(worker.Request{TenantID: tenant, MessageID: msgID, Key: key})
	if err := a.Publish(context.Background(), worker.SubjectRequested, reqBody); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case c := <-completedCh:
		if c.Status != scanner.StatusInfected {
			t.Errorf("completed status = %v, want infected", c.Status)
		}
		if c.MessageID != msgID {
			t.Errorf("completed message id = %v, want %v", c.MessageID, msgID)
		}
		if !strings.HasPrefix(c.EngineID, "clamav-") {
			t.Errorf("completed engine = %q, want clamav-* prefix", c.EngineID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for media.scan.completed")
	}

	st, en := readMediaScanStatusForE2E(t, db, msgID)
	if st != "infected" {
		t.Errorf("DB scan_status = %q, want infected", st)
	}
	if !strings.HasPrefix(en, "clamav-") {
		t.Errorf("DB scan_engine = %q, want clamav-* prefix", en)
	}
}

func TestMediaScan_E2E_CleanFile(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test (Postgres + embedded NATS); skipped under -short")
	}
	t.Parallel()

	db := freshDBForMessageMedia(t)
	store, err := pgmessagemedia.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("messagemedia.New: %v", err)
	}

	tenant, msgID, key := seedMediaScanMessage(t, db)
	blobs := &mapBlobs{m: map[string][]byte{key: []byte("hello world")}}

	scn, err := clamavadapter.New(clamavadapter.Config{Addr: "ignored", Dial: eicarClamd()}, blobs)
	if err != nil {
		t.Fatalf("clamav.New: %v", err)
	}

	url := startEmbeddedNATS(t)
	a, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL: url, ConnectTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(a.Close)
	if err := a.EnsureStream("MEDIA_E2E_CLN", []string{worker.SubjectRequested, worker.SubjectCompleted}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	pub := &publisherShim{a: a}
	h, err := worker.New(scn, store, pub, nil)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	completedCh := make(chan worker.Completed, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = a.Subscribe(ctx, worker.SubjectCompleted, "g-c-cln", "d-c-cln", time.Second,
		func(_ context.Context, d *natsadapter.Delivery) error {
			var c worker.Completed
			_ = json.Unmarshal(d.Data(), &c)
			_ = d.Ack(context.Background())
			select {
			case completedCh <- c:
			default:
			}
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe completed: %v", err)
	}
	_, err = a.Subscribe(ctx, worker.SubjectRequested, "g-w-cln", "d-w-cln", 5*time.Second,
		func(ctx context.Context, d *natsadapter.Delivery) error {
			return h.Handle(ctx, d)
		})
	if err != nil {
		t.Fatalf("subscribe requested: %v", err)
	}

	reqBody, _ := json.Marshal(worker.Request{TenantID: tenant, MessageID: msgID, Key: key})
	if err := a.Publish(context.Background(), worker.SubjectRequested, reqBody); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case c := <-completedCh:
		if c.Status != scanner.StatusClean {
			t.Errorf("completed status = %v, want clean", c.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for media.scan.completed")
	}

	st, _ := readMediaScanStatusForE2E(t, db, msgID)
	if st != "clean" {
		t.Errorf("DB scan_status = %q, want clean", st)
	}
}

// Suppress unused-warning shim for sync import in some build paths.
var _ = sync.Mutex{}
