package wasession

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEventConstructors(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	inb := newInboundEvent(tenant, InboundMessage{ExternalID: "wamid.1", Body: "oi"})
	if inb.Kind != EventInbound || inb.TenantID != tenant || inb.Inbound == nil {
		t.Fatalf("inbound event malformed: %+v", inb)
	}
	if inb.Status != nil || inb.QR != nil {
		t.Error("inbound event must not set Status/QR")
	}
	if inb.Inbound.Body != "oi" {
		t.Errorf("inbound body = %q", inb.Inbound.Body)
	}

	st := newStatusEvent(tenant, StatusChange{From: StatusDisconnected, To: StatusConnected, Reason: "up"})
	if st.Kind != EventStatus || st.Status == nil || st.Status.To != StatusConnected {
		t.Fatalf("status event malformed: %+v", st)
	}
	if st.Inbound != nil || st.QR != nil {
		t.Error("status event must not set Inbound/QR")
	}

	qr := newQREvent(tenant, QRCode{Code: NewCredential("x"), ExpiresAt: time.Unix(10, 0)})
	if qr.Kind != EventQR || qr.QR == nil || qr.QR.Code.Reveal() != "x" {
		t.Fatalf("qr event malformed: %+v", qr)
	}
	if qr.Inbound != nil || qr.Status != nil {
		t.Error("qr event must not set Inbound/Status")
	}
}
