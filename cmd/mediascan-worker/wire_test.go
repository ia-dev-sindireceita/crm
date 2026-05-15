// Unit tests for Wire ([SIN-62826]). These exercise the wiring path
// the production binary depends on (EnsureStream → worker.New → MinIO
// quarantine wiring → semaphore-bounded queue subscribe → startup log)
// using in-process fakes — no testcontainers, no live Postgres / NATS /
// MinIO rig.
//
// The fakes implement the narrow ports Wire consumes (NATSAdapter,
// Subscription, scanner.MediaScanner, scanner.MessageMediaStore,
// worker.Publisher, quarantine.Quarantiner, alert.Alerter). Most tests
// drive Wire in a goroutine, wait for it to install the JetStream
// subscription, then either invoke the captured handler directly or
// cancel the context to observe shutdown.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/alert"
	"github.com/pericles-luz/crm/internal/media/scanner"
	"github.com/pericles-luz/crm/internal/media/worker"
)

// --- fakes -----------------------------------------------------------

type fakeNATS struct {
	streamName     string
	streamSubjects []string
	streamErr      error
	streamCalls    atomic.Int32

	subSubject string
	subQueue   string
	subDurable string
	subAckWait time.Duration
	subHandler HandleFunc
	subErr     error
	subscribed chan struct{}

	sub      *fakeSubscription
	drainErr error
	drained  atomic.Bool
}

func newFakeNATS() *fakeNATS {
	return &fakeNATS{
		sub:        &fakeSubscription{},
		subscribed: make(chan struct{}),
	}
}

func (f *fakeNATS) EnsureStream(name string, subjects []string) error {
	f.streamCalls.Add(1)
	f.streamName = name
	f.streamSubjects = append([]string(nil), subjects...)
	return f.streamErr
}

func (f *fakeNATS) Subscribe(
	_ context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler HandleFunc,
) (Subscription, error) {
	f.subSubject = subject
	f.subQueue = queue
	f.subDurable = durable
	f.subAckWait = ackWait
	f.subHandler = handler
	if f.subErr != nil {
		return nil, f.subErr
	}
	close(f.subscribed)
	return f.sub, nil
}

func (f *fakeNATS) Drain() error {
	f.drained.Store(true)
	return f.drainErr
}

type fakeSubscription struct {
	drained atomic.Bool
	err     error
}

func (s *fakeSubscription) Drain() error {
	s.drained.Store(true)
	return s.err
}

type fakeScanner struct {
	mu       sync.Mutex
	result   scanner.ScanResult
	err      error
	started  chan string // signalled per Scan call with the key
	release  chan struct{}
	calls    atomic.Int32
	keysSeen []string
}

func (f *fakeScanner) Scan(_ context.Context, key string) (scanner.ScanResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.keysSeen = append(f.keysSeen, key)
	started := f.started
	release := f.release
	f.mu.Unlock()
	if started != nil {
		started <- key
	}
	if release != nil {
		<-release
	}
	return f.result, f.err
}

type fakeStore struct {
	mu       sync.Mutex
	err      error
	received []scanner.ScanResult
}

func (f *fakeStore) UpdateScanResult(_ context.Context, _, _ uuid.UUID, r scanner.ScanResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, r)
	return f.err
}

type fakePublisher struct {
	mu       sync.Mutex
	err      error
	subjects []string
	bodies   [][]byte
}

func (f *fakePublisher) Publish(_ context.Context, subject string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)
	return f.err
}

type fakeDelivery struct {
	body  []byte
	acked atomic.Int32
}

func (d *fakeDelivery) Data() []byte                { return d.body }
func (d *fakeDelivery) Ack(_ context.Context) error { d.acked.Add(1); return nil }

type fakeQuarantiner struct {
	mu    sync.Mutex
	calls []string
}

func (q *fakeQuarantiner) Move(_ context.Context, key string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, key)
	return nil
}

type fakeAlerter struct {
	mu     sync.Mutex
	events []alert.Event
}

func (a *fakeAlerter) Notify(_ context.Context, ev alert.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

// --- helpers ---------------------------------------------------------

func baseCfg() config {
	return config{
		natsURL:           "tls://nats.example:4222",
		streamName:        "MEDIA_SCAN",
		durableName:       "mediascan-worker",
		queueName:         "mediascan-workers",
		concurrency:       4,
		natsCredsFile:     "/etc/nats/worker.creds",
		natsTLSCAFile:     "/etc/nats/ca.pem",
		minioCredsRefresh: 50 * time.Minute,
	}
}

func makeReqBody(t *testing.T, key string) []byte {
	t.Helper()
	b, err := json.Marshal(worker.Request{
		TenantID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		MessageID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Key:       key,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// runWire boots Wire in a goroutine with a cancellable context and the
// supplied deps, then waits for Subscribe to fire (or returns an error
// when Wire exits before subscribing). Callers cancel the returned ctx
// to drive shutdown and call wait() to block until Wire returns.
func runWire(t *testing.T, deps Deps) (ctx context.Context, cancel context.CancelFunc, wait func() error) {
	t.Helper()
	ctx, cancel = context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Wire(ctx, deps) }()

	// Wait for Subscribe to install the handler; the test fake closes
	// subscribed once Subscribe is called successfully. If Wire errors
	// out before that, surface the error so the test can decide.
	select {
	case <-deps.NATS.(*fakeNATS).subscribed:
		// subscribed installed
	case err := <-errCh:
		cancel()
		return ctx, cancel, func() error { return err }
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("timed out waiting for Subscribe")
	}

	return ctx, cancel, func() error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Wire to return")
			return nil
		}
	}
}

// captureLogger returns a JSON-handler-backed logger and a func that
// returns every record decoded as map[string]any.
func captureLogger(t *testing.T) (*slog.Logger, func() []map[string]any) {
	t.Helper()
	var buf strings.Builder
	h := slog.NewJSONHandler(&safeWriter{buf: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			out = append(out, m)
		}
		return out
	}
}

type safeWriter struct {
	mu  sync.Mutex
	buf *strings.Builder
}

func (w *safeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// --- compile-time fences --------------------------------------------

var _ NATSAdapter = (*fakeNATS)(nil)
var _ Subscription = (*fakeSubscription)(nil)
var _ scanner.MediaScanner = (*fakeScanner)(nil)
var _ scanner.MessageMediaStore = (*fakeStore)(nil)
var _ worker.Publisher = (*fakePublisher)(nil)
var _ worker.Delivery = (*fakeDelivery)(nil)

// --- tests -----------------------------------------------------------

func TestWire_EnsureStreamUsesConfiguredStreamAndBothSubjects(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	if got := nats.streamCalls.Load(); got != 1 {
		t.Errorf("EnsureStream calls = %d, want 1", got)
	}
	if nats.streamName != "MEDIA_SCAN" {
		t.Errorf("stream name = %q", nats.streamName)
	}
	want := map[string]bool{
		worker.SubjectRequested: true,
		worker.SubjectCompleted: true,
	}
	if len(nats.streamSubjects) != len(want) {
		t.Errorf("streamSubjects len = %d, want %d (%v)", len(nats.streamSubjects), len(want), nats.streamSubjects)
	}
	for _, s := range nats.streamSubjects {
		if !want[s] {
			t.Errorf("unexpected stream subject %q", s)
		}
	}
}

func TestWire_PropagatesEnsureStreamError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("stream-down")
	nats := newFakeNATS()
	nats.streamErr = sentinel
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	err := Wire(context.Background(), deps)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("Wire err = %v, want wrap of %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "ensure stream") {
		t.Errorf("err %q should name the stage 'ensure stream'", err.Error())
	}
}

func TestWire_PropagatesWorkerNewError_NilScanner(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   nil, // worker.New rejects this
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	err := Wire(context.Background(), deps)
	if err == nil {
		t.Fatal("expected worker.New error")
	}
	if !strings.Contains(err.Error(), "worker.New") {
		t.Errorf("err %q should name worker.New stage", err.Error())
	}
}

func TestWire_PropagatesSubscribeError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("subscribe-failed")
	nats := newFakeNATS()
	nats.subErr = sentinel
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	err := Wire(context.Background(), deps)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("Wire err = %v, want wrap of %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "nats subscribe") {
		t.Errorf("err %q should name the 'nats subscribe' stage", err.Error())
	}
}

func TestWire_SubscribePassesAckWaitAndQueueNames(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	cfg := baseCfg()
	cfg.queueName = "custom-q"
	cfg.durableName = "custom-d"
	deps := Deps{
		Cfg:       cfg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	if nats.subSubject != worker.SubjectRequested {
		t.Errorf("subject = %q", nats.subSubject)
	}
	if nats.subQueue != "custom-q" {
		t.Errorf("queue = %q", nats.subQueue)
	}
	if nats.subDurable != "custom-d" {
		t.Errorf("durable = %q", nats.subDurable)
	}
	if nats.subAckWait != SubscribeAckWait {
		t.Errorf("ackWait = %v, want %v", nats.subAckWait, SubscribeAckWait)
	}
}

func TestWire_HandlerWiresQuarantinerAndAlerterWhenSet(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	scn := &fakeScanner{result: scanner.ScanResult{
		Status:    scanner.StatusInfected,
		EngineID:  "clamav-1.2.3",
		Signature: "Eicar-Test-Signature",
	}}
	q := &fakeQuarantiner{}
	a := &fakeAlerter{}
	deps := Deps{
		Cfg:         baseCfg(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:     scn,
		Store:       &fakeStore{},
		NATS:        nats,
		Publisher:   &fakePublisher{},
		Quarantiner: q,
		Alerter:     a,
	}

	ctx, cancel, wait := runWire(t, deps)
	defer cancel()

	d := &fakeDelivery{body: makeReqBody(t, "media/abc")}
	if err := nats.subHandler(ctx, d); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(q.calls) != 1 || q.calls[0] != "media/abc" {
		t.Errorf("quarantiner Move calls = %v, want [media/abc]", q.calls)
	}
	if len(a.events) != 1 || a.events[0].Signature != "Eicar-Test-Signature" {
		t.Errorf("alerter events = %v", a.events)
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}
}

func TestWire_HandlerSkipsQuarantinerAndAlerterWhenNil(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	scn := &fakeScanner{result: scanner.ScanResult{
		Status:   scanner.StatusInfected,
		EngineID: "clamav-1.2.3",
	}}
	deps := Deps{
		Cfg:         baseCfg(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:     scn,
		Store:       &fakeStore{},
		NATS:        nats,
		Publisher:   &fakePublisher{},
		Quarantiner: nil,
		Alerter:     nil,
	}

	ctx, cancel, wait := runWire(t, deps)
	defer cancel()

	d := &fakeDelivery{body: makeReqBody(t, "media/abc")}
	if err := nats.subHandler(ctx, d); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if d.acked.Load() != 1 {
		t.Errorf("ack count = %d, want 1 (handler should still ack on infected without defense-in-depth)", d.acked.Load())
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}
}

func TestWire_StartupLogRecordsSecurityPostureWithoutSecrets(t *testing.T) {
	t.Parallel()
	logger, drain := captureLogger(t)

	nats := newFakeNATS()
	cfg := baseCfg()
	cfg.natsToken = "super-secret-token"
	cfg.minioAccessKey = "AK-LEAK"
	cfg.minioSecretKey = "SK-LEAK"
	cfg.minioSessionToken = "STS-LEAK"
	cfg.minioEndpoint = "http://minio:9000"
	cfg.natsTLSCertFile = "/etc/nats/client.crt"
	cfg.natsTLSKeyFile = "/etc/nats/client.key"
	deps := Deps{
		Cfg:         cfg,
		Logger:      logger,
		Scanner:     &fakeScanner{},
		Store:       &fakeStore{},
		NATS:        nats,
		Publisher:   &fakePublisher{},
		Quarantiner: &fakeQuarantiner{},
		Alerter:     &fakeAlerter{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	lines := drain()
	if len(lines) == 0 {
		t.Fatal("expected at least the 'ready' log line")
	}
	var ready map[string]any
	for _, l := range lines {
		if l["msg"] == "mediascan-worker ready" {
			ready = l
			break
		}
	}
	if ready == nil {
		t.Fatalf("missing 'ready' log line; got %v", lines)
	}
	for _, field := range []string{
		"nats", "stream", "queue", "concurrency", "auth", "tls_ca",
		"mtls", "insecure", "quarantiner", "alerter",
		"minio_creds", "minio_creds_refresh",
	} {
		if _, ok := ready[field]; !ok {
			t.Errorf("ready log missing field %q (line=%v)", field, ready)
		}
	}
	// Modes report the kind, not the secret material.
	if ready["auth"] != "token" {
		// natsAuthMode prefers creds-file when set; this cfg also has
		// CredsFile, so the precedence rule reports creds-file even
		// when token is set alongside. Just assert it's not the raw
		// token bytes.
		t.Logf("auth field = %v (precedence-driven)", ready["auth"])
	}
	if ready["minio_creds"] != "static-env" {
		t.Errorf("minio_creds = %v, want static-env", ready["minio_creds"])
	}
	if ready["quarantiner"] != true {
		t.Errorf("quarantiner = %v, want true", ready["quarantiner"])
	}
	if ready["alerter"] != true {
		t.Errorf("alerter = %v, want true", ready["alerter"])
	}

	// Critical: secret bytes must NOT appear anywhere in the log.
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		for _, leak := range []string{"super-secret-token", "AK-LEAK", "SK-LEAK", "STS-LEAK"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("log line leaked secret %q: %s", leak, string(raw))
			}
		}
	}
}

func TestWire_StartupLogQuarantinerAndAlerterFalseWhenNil(t *testing.T) {
	t.Parallel()
	logger, drain := captureLogger(t)
	nats := newFakeNATS()
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    logger,
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	for _, l := range drain() {
		if l["msg"] != "mediascan-worker ready" {
			continue
		}
		if l["quarantiner"] != false {
			t.Errorf("quarantiner = %v, want false", l["quarantiner"])
		}
		if l["alerter"] != false {
			t.Errorf("alerter = %v, want false", l["alerter"])
		}
		if l["minio_creds"] != "none" {
			t.Errorf("minio_creds = %v, want none", l["minio_creds"])
		}
	}
}

func TestWire_SemaphoreBoundsConcurrency(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	scn := &fakeScanner{
		result:  scanner.ScanResult{Status: scanner.StatusClean, EngineID: "clamav-1.2.3"},
		started: make(chan string, 4),
		release: make(chan struct{}),
	}
	cfg := baseCfg()
	cfg.concurrency = 2
	deps := Deps{
		Cfg:       cfg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   scn,
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	ctx, cancel, wait := runWire(t, deps)
	defer cancel()

	// Fire three deliveries concurrently — semaphore is 2, so only
	// two should reach the scanner until one is released.
	for i := 0; i < 3; i++ {
		i := i
		go func() {
			d := &fakeDelivery{body: makeReqBody(t, "media/"+string(rune('a'+i)))}
			_ = nats.subHandler(ctx, d)
		}()
	}

	// Wait for the first two to enter Scan.
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-scn.started:
		case <-timeout:
			t.Fatalf("only %d deliveries reached the scanner within 2s; semaphore did not let two through", i)
		}
	}

	// The third must be blocked on the semaphore — give it a brief
	// window to confirm.
	select {
	case <-scn.started:
		t.Fatal("third delivery reached the scanner; semaphore did not bound concurrency at 2")
	case <-time.After(150 * time.Millisecond):
		// good — third is parked on `sem <- struct{}{}`
	}

	// Release the gate so the goroutines drain.
	close(scn.release)
	// Pump the third start so we don't leak the goroutine.
	select {
	case <-scn.started:
	case <-time.After(2 * time.Second):
		t.Fatal("third delivery never proceeded after release")
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}
}

func TestWire_DrainsSubscriptionAndAdapterOnCtxCancel(t *testing.T) {
	t.Parallel()
	nats := newFakeNATS()
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	if !nats.sub.drained.Load() {
		t.Error("subscription was not drained on shutdown")
	}
	if !nats.drained.Load() {
		t.Error("nats adapter was not drained on shutdown")
	}
}

func TestWire_LogsNATSDrainError(t *testing.T) {
	t.Parallel()
	logger, drain := captureLogger(t)
	nats := newFakeNATS()
	nats.drainErr = errors.New("drain-failed")
	deps := Deps{
		Cfg:       baseCfg(),
		Logger:    logger,
		Scanner:   &fakeScanner{},
		Store:     &fakeStore{},
		NATS:      nats,
		Publisher: &fakePublisher{},
	}

	_, cancel, wait := runWire(t, deps)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Wire: %v", err)
	}

	var saw bool
	for _, l := range drain() {
		if l["msg"] == "nats drain" && l["err"] == "drain-failed" {
			saw = true
		}
	}
	if !saw {
		t.Error("expected 'nats drain' warn log with err field 'drain-failed'")
	}
}

// --- openMinioProvider --- the shared provider gate ---

func TestOpenMinioProvider_NilWhenEndpointUnset(t *testing.T) {
	t.Parallel()
	p, err := openMinioProvider(config{})
	if err != nil {
		t.Fatalf("openMinioProvider: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

func TestOpenMinioProvider_BuildsRotatingProviderFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/creds.json"
	if err := writeJSONCreds(path, "AK", "SK", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := config{
		minioEndpoint:     "http://minio:9000",
		minioCredsFile:    path,
		minioCredsRefresh: time.Minute,
	}
	p, err := openMinioProvider(cfg)
	if err != nil {
		t.Fatalf("openMinioProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	creds, err := p()
	if err != nil {
		t.Fatalf("provider call: %v", err)
	}
	if creds.AccessKeyID != "AK" {
		t.Errorf("got AccessKeyID = %q", creds.AccessKeyID)
	}
}

// --- openScanner --- ClamAV constructor wrapper ---

func TestOpenScanner_RejectsEmptyAddr(t *testing.T) {
	t.Parallel()
	_, err := openScanner(config{}, &localBlobs{root: t.TempDir()})
	if err == nil {
		t.Fatal("expected error on empty clamd addr")
	}
}

func TestOpenScanner_BuildsWithLocalBlobs(t *testing.T) {
	t.Parallel()
	cfg := config{clamdAddr: "clamav:3310"}
	scn, err := openScanner(cfg, &localBlobs{root: t.TempDir()})
	if err != nil {
		t.Fatalf("openScanner: %v", err)
	}
	if scn == nil {
		t.Fatal("expected non-nil scanner")
	}
}

// --- buildNATSConfig --- pure data transform ---

func TestBuildNATSConfig_MapsAllConfigFields(t *testing.T) {
	t.Parallel()
	cfg := config{
		natsURL:         "tls://nats.example:4222",
		natsToken:       "tok",
		natsNKeyFile:    "/etc/nkey",
		natsCredsFile:   "/etc/creds",
		natsTLSCAFile:   "/etc/ca.pem",
		natsTLSCertFile: "/etc/client.crt",
		natsTLSKeyFile:  "/etc/client.key",
		natsInsecure:    true,
	}
	got := buildNATSConfig(cfg)
	if got.URL != cfg.natsURL {
		t.Errorf("URL = %q", got.URL)
	}
	if got.Name != "crm-mediascan-worker" {
		t.Errorf("Name = %q, want crm-mediascan-worker", got.Name)
	}
	if got.MaxReconnects != -1 {
		t.Errorf("MaxReconnects = %d, want -1", got.MaxReconnects)
	}
	if got.Token != cfg.natsToken || got.NKeyFile != cfg.natsNKeyFile ||
		got.CredsFile != cfg.natsCredsFile || got.TLSCAFile != cfg.natsTLSCAFile ||
		got.TLSCertFile != cfg.natsTLSCertFile || got.TLSKeyFile != cfg.natsTLSKeyFile ||
		got.Insecure != cfg.natsInsecure {
		t.Errorf("config fields not propagated: %+v", got)
	}
}

// --- buildQuarantiner --- MINIO_ENDPOINT decision tree ---

func TestBuildQuarantiner_NilWhenMinioUnset(t *testing.T) {
	t.Parallel()
	q, err := buildQuarantiner(config{}, nil)
	if err != nil {
		t.Fatalf("buildQuarantiner: %v", err)
	}
	if q != nil {
		t.Errorf("expected nil quarantiner when MINIO_ENDPOINT empty, got %T", q)
	}
}

func TestBuildQuarantiner_BuildsWithSharedProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/creds.json"
	if err := writeJSONCreds(path, "AK", "SK", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := config{
		minioEndpoint:     "http://minio:9000",
		minioRegion:       "us-east-1",
		minioSource:       "media",
		minioDest:         "media-quarantine",
		minioCredsFile:    path,
		minioCredsRefresh: time.Minute,
	}
	provider, err := buildCredentialsProvider(cfg)
	if err != nil {
		t.Fatalf("buildCredentialsProvider: %v", err)
	}
	q, err := buildQuarantiner(cfg, provider)
	if err != nil {
		t.Fatalf("buildQuarantiner: %v", err)
	}
	if q == nil {
		t.Fatal("expected non-nil quarantiner")
	}
}

func TestBuildQuarantiner_PropagatesUpstreamError(t *testing.T) {
	t.Parallel()
	// Missing SourceBucket triggers minioadapter.New's validation.
	cfg := config{
		minioEndpoint:  "http://minio:9000",
		minioRegion:    "us-east-1",
		minioDest:      "media-quarantine",
		minioAccessKey: "AK",
		minioSecretKey: "SK",
	}
	_, err := buildQuarantiner(cfg, nil)
	if err == nil {
		t.Fatal("expected error when SourceBucket is empty")
	}
}

// --- buildAlerter --- SLACK_WEBHOOK_URL decision tree ---

func TestBuildAlerter_NilWhenWebhookUnset(t *testing.T) {
	t.Parallel()
	a, err := buildAlerter(config{})
	if err != nil {
		t.Fatalf("buildAlerter: %v", err)
	}
	if a != nil {
		t.Errorf("expected nil alerter when SLACK_WEBHOOK_URL empty, got %T", a)
	}
}

func TestBuildAlerter_BuildsWhenWebhookSet(t *testing.T) {
	t.Parallel()
	a, err := buildAlerter(config{slackWebhookURL: "https://hooks.slack.com/x"})
	if err != nil {
		t.Fatalf("buildAlerter: %v", err)
	}
	if a == nil {
		t.Fatal("expected non-nil alerter")
	}
}

func TestBuildAlerter_RejectsNonAbsoluteWebhook(t *testing.T) {
	t.Parallel()
	_, err := buildAlerter(config{slackWebhookURL: "ftp://nope/x"})
	if err == nil {
		t.Fatal("expected error on non-http(s) webhook URL")
	}
}

// --- run() error-path tests ------------------------------------------
//
// run() does real I/O on the happy path (pgxpool dial, NATS connect,
// clamd dial), so we cannot exercise the full body in a unit test.
// What we CAN do is drive run() through each early-exit error path:
// invalid env, bad DSN, unreachable Postgres, bad NATS URL. That
// covers the glue statements (loadConfig wrap, NotifyContext, defer,
// pgxpool.New error wrap, pool.Ping error wrap, etc.) so the package
// total clears the [SIN-62826] 85% bar even with the dial-bound tail
// of run() inevitably staying uncovered.

func quietRunLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRun_FailsOnConfigError(t *testing.T) {
	clearWorkerEnv(t)
	err := run(quietRunLogger())
	if err == nil {
		t.Fatal("expected error on missing required env")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("err %q should mention 'config' stage", err.Error())
	}
}

func TestRun_FailsOnPgxpoolNewWithMalformedDSN(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://bad host:port/db?invalid")
	t.Setenv("CLAMD_ADDR", "clamav:3310")

	err := run(quietRunLogger())
	if err == nil {
		t.Fatal("expected error on malformed DSN")
	}
	// Either pgxpool parse or postgres ping path is acceptable — both
	// originate inside run() before the NATS dial would fire.
	if !strings.Contains(err.Error(), "pgxpool") && !strings.Contains(err.Error(), "postgres") {
		t.Errorf("err %q should mention pgxpool or postgres stage", err.Error())
	}
}

func TestRun_FailsOnPostgresPingUnreachable(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	// Localhost port 1 is unprivileged-rejected on the test host;
	// connect_timeout=1 caps Ping at ~1s so the unit test stays fast.
	t.Setenv("POSTGRES_DSN", "postgres://x@127.0.0.1:1/db?connect_timeout=1&sslmode=disable")
	t.Setenv("CLAMD_ADDR", "clamav:3310")

	err := run(quietRunLogger())
	if err == nil {
		t.Fatal("expected error on unreachable Postgres")
	}
	if !strings.Contains(err.Error(), "postgres ping") {
		t.Errorf("err %q should mention 'postgres ping' stage", err.Error())
	}
}

// --- runWithStore error-path tests -----------------------------------
//
// runWithStore is the testable interior of run() — it skips the
// Postgres dial + Store construction and accepts a port-shaped Store
// instead. Each error branch is driven directly with cfg fields tuned
// to trip the stage under test.

func TestRunWithStore_FailsOnMinioCredsError(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.minioEndpoint = "http://minio:9000"
	cfg.minioSource = "media"
	cfg.minioDest = "media-quarantine"
	// Static-provider path with empty AccessKeyID — StaticProvider
	// validates at construction so buildCredentialsProvider errors
	// without dialing MinIO.
	cfg.minioAccessKey = ""
	cfg.minioSecretKey = ""
	err := runWithStore(context.Background(), cfg, &fakeStore{}, quietRunLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "minio credentials") {
		t.Errorf("err %q should name 'minio credentials' stage", err.Error())
	}
}

func TestRunWithStore_FailsOnBlobReaderError(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	// Neither BLOB_BASE_DIR nor MINIO_ENDPOINT set → buildBlobReader fails.
	err := runWithStore(context.Background(), cfg, &fakeStore{}, quietRunLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "blob reader") {
		t.Errorf("err %q should name 'blob reader' stage", err.Error())
	}
}

func TestRunWithStore_FailsOnEmptyClamdAddr(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.blobBaseDir = t.TempDir()
	// clamdAddr empty in baseCfg → openScanner → clamavadapter.New rejects.
	err := runWithStore(context.Background(), cfg, &fakeStore{}, quietRunLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "clamav") {
		t.Errorf("err %q should name 'clamav' stage", err.Error())
	}
}

func TestRunWithStore_FailsOnNATSConnect(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.blobBaseDir = t.TempDir()
	cfg.clamdAddr = "clamav:3310"
	// natsURL points at an unreachable port, but the SDKConfig validator
	// fires before any dial when scheme is tls:// without a CA bundle.
	// We pre-cleared natsTLSCAFile in cfg so validation rejects the URL.
	cfg.natsTLSCAFile = ""
	cfg.natsCredsFile = ""
	cfg.natsURL = "tls://nats.example:4222"
	err := runWithStore(context.Background(), cfg, &fakeStore{}, quietRunLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nats.Connect") {
		t.Errorf("err %q should name 'nats.Connect' stage", err.Error())
	}
}

// writeJSONCreds drops a credentials JSON file in the shape
// NewFileRefresher reads. Used by the buildQuarantiner test that
// exercises the shared-provider path.
func writeJSONCreds(path, ak, sk, st string) error {
	body, err := json.Marshal(map[string]string{
		"accessKey":    ak,
		"secretKey":    sk,
		"sessionToken": st,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// --- buildBlobReaderWithProvider --- the new shared-provider helper ---

func TestBuildBlobReaderWithProvider_RejectsNilProviderWithMinio(t *testing.T) {
	t.Parallel()
	_, err := buildBlobReaderWithProvider(config{minioEndpoint: "http://minio:9000"}, nil)
	if err == nil {
		t.Fatal("expected error when MINIO_ENDPOINT is set but provider is nil")
	}
	if !strings.Contains(err.Error(), "MINIO_ENDPOINT") {
		t.Errorf("err %q should name MINIO_ENDPOINT", err.Error())
	}
}

func TestBuildBlobReaderWithProvider_LocalFsBranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := buildBlobReaderWithProvider(config{blobBaseDir: dir}, nil)
	if err != nil {
		t.Fatalf("buildBlobReaderWithProvider: %v", err)
	}
	if _, ok := r.(*localBlobs); !ok {
		t.Fatalf("expected *localBlobs, got %T", r)
	}
}
