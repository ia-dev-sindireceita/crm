package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/consent"
	"github.com/pericles-luz/crm/internal/wasession"
	webwasession "github.com/pericles-luz/crm/internal/web/wasession"
)

// --- managerProvisioner ----------------------------------------------------

type fakeController struct {
	status     wasession.Status
	present    bool
	startErr   error
	stopErr    error
	startCalls int
	stopCalls  int
}

func (f *fakeController) StartSession(_ context.Context, _ uuid.UUID) error {
	f.startCalls++
	return f.startErr
}
func (f *fakeController) StopSession(_ uuid.UUID) error {
	f.stopCalls++
	return f.stopErr
}
func (f *fakeController) Status(_ uuid.UUID) (wasession.Status, bool) {
	return f.status, f.present
}

type fakeQRReader struct {
	qr wasession.QRCode
	ok bool
}

func (f fakeQRReader) Get(_ uuid.UUID) (wasession.QRCode, bool) { return f.qr, f.ok }

func TestManagerProvisioner_Snapshot_ActiveWithQR(t *testing.T) {
	t.Parallel()
	ctrl := &fakeController{status: wasession.StatusPairing, present: true}
	qr := fakeQRReader{qr: wasession.QRCode{Code: wasession.NewCredential("pair-1")}, ok: true}
	p := managerProvisioner{ctrl: ctrl, qr: qr}

	snap, err := p.Snapshot(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Snapshot err = %v", err)
	}
	if !snap.Active || snap.Status != "pairing" {
		t.Fatalf("snap = %+v, want active pairing", snap)
	}
	if snap.QRPayload != "pair-1" {
		t.Fatalf("QRPayload = %q, want pair-1", snap.QRPayload)
	}
}

func TestManagerProvisioner_Snapshot_Inactive(t *testing.T) {
	t.Parallel()
	p := managerProvisioner{ctrl: &fakeController{present: false}, qr: fakeQRReader{}}
	snap, _ := p.Snapshot(context.Background(), uuid.New())
	if snap.Active || snap.Status != "" || snap.QRPayload != "" {
		t.Fatalf("snap = %+v, want inactive/empty", snap)
	}
}

func TestManagerProvisioner_Connect_IdempotentOnExists(t *testing.T) {
	t.Parallel()
	ctrl := &fakeController{startErr: wasession.ErrSessionExists}
	p := managerProvisioner{ctrl: ctrl}
	if err := p.Connect(context.Background(), uuid.New()); err != nil {
		t.Fatalf("Connect on already-running = %v, want nil (idempotent)", err)
	}
	if ctrl.startCalls != 1 {
		t.Fatalf("startCalls = %d, want 1", ctrl.startCalls)
	}
}

func TestManagerProvisioner_Connect_PropagatesError(t *testing.T) {
	t.Parallel()
	ctrl := &fakeController{startErr: errors.New("boom")}
	p := managerProvisioner{ctrl: ctrl}
	if err := p.Connect(context.Background(), uuid.New()); err == nil {
		t.Fatal("Connect err = nil, want propagated start error")
	}
}

func TestManagerProvisioner_Disconnect_IdempotentOnNotFound(t *testing.T) {
	t.Parallel()
	ctrl := &fakeController{stopErr: wasession.ErrSessionNotFound}
	p := managerProvisioner{ctrl: ctrl}
	if err := p.Disconnect(context.Background(), uuid.New()); err != nil {
		t.Fatalf("Disconnect on absent = %v, want nil (idempotent)", err)
	}
}

func TestManagerProvisioner_Disconnect_PropagatesError(t *testing.T) {
	t.Parallel()
	ctrl := &fakeController{stopErr: errors.New("boom")}
	p := managerProvisioner{ctrl: ctrl}
	if err := p.Disconnect(context.Background(), uuid.New()); err == nil {
		t.Fatal("Disconnect err = nil, want propagated stop error")
	}
}

// --- consentGateAdapter ----------------------------------------------------

type fakeWASessConsentRecorder struct {
	latest    *consent.ConsentRecord
	latestErr error
	recordErr error
	recorded  *consent.ConsentRecord
}

func (f *fakeWASessConsentRecorder) Record(_ context.Context, rec consent.ConsentRecord) (consent.ConsentRecord, bool, error) {
	if f.recordErr != nil {
		return consent.ConsentRecord{}, false, f.recordErr
	}
	cp := rec
	f.recorded = &cp
	return rec, true, nil
}

func (f *fakeWASessConsentRecorder) Latest(_ context.Context, _ uuid.UUID, _ consent.Subject, _ consent.Purpose) (*consent.ConsentRecord, error) {
	return f.latest, f.latestErr
}

func TestConsentGateAdapter_Latest_NoneIsNotGranted(t *testing.T) {
	t.Parallel()
	a := consentGateAdapter{reg: &fakeWASessConsentRecorder{latest: nil}}
	st, err := a.Latest(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("Latest err = %v", err)
	}
	if st.Granted {
		t.Fatal("Granted = true for no record, want false")
	}
}

func TestConsentGateAdapter_Latest_GrantedMapsFields(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC()
	rec := &consent.ConsentRecord{Granted: true, Version: webwasession.NoticeVersion, GrantedAt: at}
	a := consentGateAdapter{reg: &fakeWASessConsentRecorder{latest: rec}}
	st, _ := a.Latest(context.Background(), uuid.New(), uuid.New())
	if !st.Granted || st.Version != webwasession.NoticeVersion || !st.At.Equal(at) {
		t.Fatalf("state = %+v, want granted/version/at mapped", st)
	}
}

func TestConsentGateAdapter_Latest_RevokedIsNotGranted(t *testing.T) {
	t.Parallel()
	revoked := time.Now().UTC()
	rec := &consent.ConsentRecord{Granted: true, Version: webwasession.NoticeVersion, RevokedAt: &revoked}
	a := consentGateAdapter{reg: &fakeWASessConsentRecorder{latest: rec}}
	st, _ := a.Latest(context.Background(), uuid.New(), uuid.New())
	if st.Granted {
		t.Fatal("revoked grant reported as Granted, want false")
	}
}

func TestConsentGateAdapter_Latest_Error(t *testing.T) {
	t.Parallel()
	a := consentGateAdapter{reg: &fakeWASessConsentRecorder{latestErr: errors.New("down")}}
	if _, err := a.Latest(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("Latest err = nil, want propagated")
	}
}

func TestConsentGateAdapter_Record_MapsToToSPurpose(t *testing.T) {
	t.Parallel()
	rec := &fakeWASessConsentRecorder{}
	a := consentGateAdapter{reg: rec}
	tenant, user := uuid.New(), uuid.New()
	err := a.Record(context.Background(), webwasession.ConsentInput{
		TenantID: tenant, UserID: user, Version: webwasession.NoticeVersion, UserAgent: "UA/1",
	})
	if err != nil {
		t.Fatalf("Record err = %v", err)
	}
	if rec.recorded == nil {
		t.Fatal("nothing recorded")
	}
	got := rec.recorded
	if got.Purpose != consent.PurposeTermsOfService {
		t.Errorf("Purpose = %q, want terms_of_service", got.Purpose)
	}
	if got.Subject.Type != consent.SubjectUser || got.Subject.ID != user.String() {
		t.Errorf("Subject = %+v, want user/%s", got.Subject, user)
	}
	if !got.Granted || got.Version != webwasession.NoticeVersion || got.TenantID != tenant {
		t.Errorf("record = %+v, want granted/version/tenant set", got)
	}
}

func TestConsentGateAdapter_Record_Error(t *testing.T) {
	t.Parallel()
	a := consentGateAdapter{reg: &fakeWASessConsentRecorder{recordErr: errors.New("db")}}
	if err := a.Record(context.Background(), webwasession.ConsentInput{}); err == nil {
		t.Fatal("Record err = nil, want propagated")
	}
}

// --- pump QR caching -------------------------------------------------------

type recordingQRSink struct {
	puts    map[uuid.UUID]wasession.QRCode
	cleared []uuid.UUID
}

func newRecordingQRSink() *recordingQRSink {
	return &recordingQRSink{puts: map[uuid.UUID]wasession.QRCode{}}
}
func (s *recordingQRSink) Put(t uuid.UUID, qr wasession.QRCode) { s.puts[t] = qr }
func (s *recordingQRSink) Clear(t uuid.UUID)                    { s.cleared = append(s.cleared, t) }

func TestPumpWASessionInbound_CachesQRThenClearsOnConnect(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ch := make(chan wasession.Event, 4)
	ch <- wasession.Event{Kind: wasession.EventQR, TenantID: tenant, QR: &wasession.QRCode{Code: wasession.NewCredential("code-1")}}
	ch <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusConnected}}
	close(ch)

	sink := newRecordingQRSink()
	pumpWASessionInbound(context.Background(), ch, &recordingReceiver{}, nil, sink)

	if got, ok := sink.puts[tenant]; !ok || got.Code.Reveal() != "code-1" {
		t.Fatalf("QR not cached: %+v ok=%v", sink.puts, ok)
	}
	if len(sink.cleared) != 1 || sink.cleared[0] != tenant {
		t.Fatalf("QR not cleared on connect: %v", sink.cleared)
	}
}

func TestPumpWASessionInbound_ClearsQROnBan(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ch := make(chan wasession.Event, 2)
	ch <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusBanned}}
	close(ch)

	sink := newRecordingQRSink()
	pumpWASessionInbound(context.Background(), ch, &recordingReceiver{}, nil, sink)
	if len(sink.cleared) != 1 {
		t.Fatalf("banned status should clear QR, cleared=%v", sink.cleared)
	}
}

func TestBuildWASessionUIHandler_NilProvisioner_Disabled(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWASessionUIHandler(context.Background(), func(string) string { return "" }, nil)
	defer cleanup()
	if h != nil {
		t.Fatal("handler non-nil with nil provisioner, want disabled")
	}
}
