package pix_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing/pix"
)

var (
	tNow       = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tExpires   = tNow.Add(30 * time.Minute) // typical PIX TTL
	qrCode     = "data:image/png;base64,AAAA"
	copyPaste  = "00020126360014BR.GOV.BCB.PIX..."
	externalID = "PSP-TXID-0001"
)

func newPending(t *testing.T) *pix.PIXCharge {
	t.Helper()
	c, err := pix.NewCharge(uuid.New(), uuid.New(), qrCode, copyPaste, tExpires, tNow)
	if err != nil {
		t.Fatalf("NewCharge: %v", err)
	}
	return c
}

func TestNewCharge(t *testing.T) {
	tenant := uuid.New()
	invoice := uuid.New()

	tests := []struct {
		name      string
		tenant    uuid.UUID
		invoice   uuid.UUID
		qr        string
		cp        string
		expires   time.Time
		now       time.Time
		wantErr   error
		assertion func(t *testing.T, c *pix.PIXCharge)
	}{
		{
			name:    "valid pending",
			tenant:  tenant,
			invoice: invoice,
			qr:      qrCode,
			cp:      copyPaste,
			expires: tExpires,
			now:     tNow,
			assertion: func(t *testing.T, c *pix.PIXCharge) {
				if c.Status() != pix.StatusPending {
					t.Errorf("status = %s, want pending", c.Status())
				}
				if c.ExternalID() != "" {
					t.Errorf("external_id should be empty on fresh charge, got %q", c.ExternalID())
				}
				if c.PaidAt() != nil {
					t.Errorf("paid_at should be nil on fresh charge")
				}
				if c.TenantID() != tenant {
					t.Errorf("tenant id mismatch")
				}
				if c.InvoiceID() != invoice {
					t.Errorf("invoice id mismatch")
				}
				if c.QRCode() != qrCode {
					t.Errorf("qr_code mismatch")
				}
				if c.CopyPaste() != copyPaste {
					t.Errorf("copy_paste mismatch")
				}
				if !c.ExpiresAt().Equal(tExpires) {
					t.Errorf("expires_at = %s, want %s", c.ExpiresAt(), tExpires)
				}
				if !c.CreatedAt().Equal(tNow) {
					t.Errorf("created_at = %s, want %s", c.CreatedAt(), tNow)
				}
				if !c.UpdatedAt().Equal(tNow) {
					t.Errorf("updated_at = %s, want %s", c.UpdatedAt(), tNow)
				}
				if c.ID() == uuid.Nil {
					t.Errorf("id should be generated, got uuid.Nil")
				}
				if c.IsTerminal() {
					t.Errorf("fresh pending charge should not be terminal")
				}
			},
		},
		{name: "zero tenant", tenant: uuid.Nil, invoice: invoice, qr: qrCode, cp: copyPaste, expires: tExpires, now: tNow, wantErr: pix.ErrZeroTenant},
		{name: "zero invoice", tenant: tenant, invoice: uuid.Nil, qr: qrCode, cp: copyPaste, expires: tExpires, now: tNow, wantErr: pix.ErrZeroInvoice},
		{name: "empty qr_code", tenant: tenant, invoice: invoice, qr: "", cp: copyPaste, expires: tExpires, now: tNow, wantErr: pix.ErrEmptyQRCode},
		{name: "empty copy_paste", tenant: tenant, invoice: invoice, qr: qrCode, cp: "", expires: tExpires, now: tNow, wantErr: pix.ErrEmptyCopyPaste},
		{name: "expires equals now", tenant: tenant, invoice: invoice, qr: qrCode, cp: copyPaste, expires: tNow, now: tNow, wantErr: pix.ErrExpiresAtInPast},
		{name: "expires before now", tenant: tenant, invoice: invoice, qr: qrCode, cp: copyPaste, expires: tNow.Add(-time.Hour), now: tNow, wantErr: pix.ErrExpiresAtInPast},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := pix.NewCharge(tc.tenant, tc.invoice, tc.qr, tc.cp, tc.expires, tc.now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.assertion != nil {
				tc.assertion(t, c)
			}
		})
	}
}

func TestHydrateCharge(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	invoice := uuid.New()
	paid := tNow.Add(10 * time.Minute)

	c := pix.HydrateCharge(
		id, tenant, invoice,
		externalID, qrCode, copyPaste,
		pix.StatusPaid,
		&paid,
		tExpires, tNow, paid,
	)

	if c.ID() != id {
		t.Errorf("ID mismatch")
	}
	if c.TenantID() != tenant {
		t.Errorf("TenantID mismatch")
	}
	if c.InvoiceID() != invoice {
		t.Errorf("InvoiceID mismatch")
	}
	if c.ExternalID() != externalID {
		t.Errorf("ExternalID mismatch")
	}
	if c.QRCode() != qrCode {
		t.Errorf("QRCode mismatch")
	}
	if c.CopyPaste() != copyPaste {
		t.Errorf("CopyPaste mismatch")
	}
	if c.Status() != pix.StatusPaid {
		t.Errorf("status = %s, want paid", c.Status())
	}
	if c.PaidAt() == nil || !c.PaidAt().Equal(paid) {
		t.Errorf("paid_at mismatch: got %v, want %v", c.PaidAt(), paid)
	}
	if !c.ExpiresAt().Equal(tExpires) {
		t.Errorf("expires_at mismatch")
	}
	if !c.CreatedAt().Equal(tNow) {
		t.Errorf("created_at mismatch")
	}
	if !c.UpdatedAt().Equal(paid) {
		t.Errorf("updated_at mismatch")
	}
	if !c.IsTerminal() {
		t.Errorf("hydrated paid charge should be terminal")
	}
}

func TestAttachExternalID(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T) *pix.PIXCharge
		externalID string
		wantErr    error
		wantState  pix.Status
	}{
		{
			name: "valid on pending",
			setup: func(t *testing.T) *pix.PIXCharge {
				return newPending(t)
			},
			externalID: externalID,
			wantState:  pix.StatusPending,
		},
		{
			name: "empty external id",
			setup: func(t *testing.T) *pix.PIXCharge {
				return newPending(t)
			},
			externalID: "",
			wantErr:    pix.ErrEmptyExternalID,
		},
		{
			name: "already set",
			setup: func(t *testing.T) *pix.PIXCharge {
				c := newPending(t)
				if err := c.AttachExternalID(externalID, tNow); err != nil {
					t.Fatalf("first attach: %v", err)
				}
				return c
			},
			externalID: "DIFFERENT-ID",
			wantErr:    pix.ErrExternalIDAlreadySet,
		},
		{
			name: "after paid is invalid transition",
			setup: func(t *testing.T) *pix.PIXCharge {
				c := newPending(t)
				if _, err := c.MarkPaid(tNow); err != nil {
					t.Fatalf("MarkPaid: %v", err)
				}
				return c
			},
			externalID: externalID,
			wantErr:    pix.ErrInvalidTransition,
		},
		{
			name: "after cancel is invalid transition",
			setup: func(t *testing.T) *pix.PIXCharge {
				c := newPending(t)
				if _, err := c.Cancel(tNow); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
				return c
			},
			externalID: externalID,
			wantErr:    pix.ErrInvalidTransition,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.setup(t)
			err := c.AttachExternalID(tc.externalID, tNow.Add(time.Second))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.ExternalID() != tc.externalID {
				t.Errorf("external_id = %q, want %q", c.ExternalID(), tc.externalID)
			}
			if !c.UpdatedAt().Equal(tNow.Add(time.Second)) {
				t.Errorf("updated_at not bumped: got %s", c.UpdatedAt())
			}
		})
	}
}

// TestMarkPaid_Transitions exercises every (from-status, MarkPaid) pair
// so the state machine is fully covered. Idempotency for duplicate
// `paid` webhooks (AC #1) is the second case in the table.
func TestMarkPaid_Transitions(t *testing.T) {
	tests := []struct {
		name        string
		from        pix.Status
		wantChanged bool
		wantErr     error
		wantStatus  pix.Status
	}{
		{name: "pending becomes paid", from: pix.StatusPending, wantChanged: true, wantStatus: pix.StatusPaid},
		{name: "paid is idempotent no-op", from: pix.StatusPaid, wantChanged: false, wantStatus: pix.StatusPaid},
		{name: "expired rejects mark paid", from: pix.StatusExpired, wantErr: pix.ErrInvalidTransition, wantStatus: pix.StatusExpired},
		{name: "cancelled rejects mark paid", from: pix.StatusCancelled, wantErr: pix.ErrInvalidTransition, wantStatus: pix.StatusCancelled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := chargeInStatus(t, tc.from)
			before := c.UpdatedAt()
			changed, err := c.MarkPaid(tNow.Add(time.Hour))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if c.Status() != tc.wantStatus {
					t.Errorf("status changed on failed transition: got %s, want %s", c.Status(), tc.wantStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if c.Status() != tc.wantStatus {
				t.Errorf("status = %s, want %s", c.Status(), tc.wantStatus)
			}
			if changed {
				if c.PaidAt() == nil {
					t.Errorf("paid_at should be set on transition")
				} else if !c.PaidAt().Equal(tNow.Add(time.Hour)) {
					t.Errorf("paid_at = %s, want %s", c.PaidAt(), tNow.Add(time.Hour))
				}
				if !c.UpdatedAt().After(before) {
					t.Errorf("updated_at should advance on transition")
				}
			} else if tc.from == pix.StatusPaid {
				// Idempotent no-op: paid_at must NOT be rewritten.
				if c.PaidAt() == nil {
					t.Fatalf("hydrated paid charge missing paid_at — test setup bug")
				}
				if c.PaidAt().Equal(tNow.Add(time.Hour)) {
					t.Errorf("idempotent MarkPaid overwrote paid_at")
				}
				if !c.UpdatedAt().Equal(before) {
					t.Errorf("idempotent MarkPaid bumped updated_at: got %s, want %s", c.UpdatedAt(), before)
				}
			}
		})
	}
}

// TestMarkPaid_Idempotent_TableInvariant is the AC #1 invariant: a
// duplicate `paid` for the same (external_id, event_type) MUST NOT
// transition twice. We model "duplicate webhook" as a second MarkPaid
// call on the same aggregate; the dedup at the EventLog layer is
// covered by TestReconciler_DuplicateEvent.
func TestMarkPaid_Idempotent_TableInvariant(t *testing.T) {
	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	firstAt := tNow.Add(5 * time.Minute)
	changed1, err := c.MarkPaid(firstAt)
	if err != nil || !changed1 {
		t.Fatalf("first MarkPaid: changed=%v err=%v", changed1, err)
	}
	if c.Status() != pix.StatusPaid {
		t.Fatalf("status after first MarkPaid = %s, want paid", c.Status())
	}
	paidAtAfterFirst := *c.PaidAt()
	updatedAfterFirst := c.UpdatedAt()

	// Replay the same logical webhook delivery — duplicate.
	secondAt := tNow.Add(10 * time.Minute)
	changed2, err := c.MarkPaid(secondAt)
	if err != nil {
		t.Fatalf("duplicate MarkPaid returned error: %v", err)
	}
	if changed2 {
		t.Errorf("duplicate MarkPaid reported changed=true; AC #1 violated")
	}
	if !c.PaidAt().Equal(paidAtAfterFirst) {
		t.Errorf("paid_at rewritten by duplicate: got %s, want %s", c.PaidAt(), paidAtAfterFirst)
	}
	if !c.UpdatedAt().Equal(updatedAfterFirst) {
		t.Errorf("updated_at rewritten by duplicate")
	}
}

func TestExpire_Transitions(t *testing.T) {
	tests := []struct {
		name        string
		from        pix.Status
		now         time.Time
		wantChanged bool
		wantErr     error
		wantStatus  pix.Status
	}{
		{
			name:        "pending after ttl elapses",
			from:        pix.StatusPending,
			now:         tExpires.Add(time.Second),
			wantChanged: true,
			wantStatus:  pix.StatusExpired,
		},
		{
			name:       "pending before ttl",
			from:       pix.StatusPending,
			now:        tExpires.Add(-time.Second),
			wantErr:    pix.ErrTTLNotElapsed,
			wantStatus: pix.StatusPending,
		},
		{
			name:       "pending at exact expires_at boundary still rejects",
			from:       pix.StatusPending,
			now:        tExpires,
			wantErr:    pix.ErrTTLNotElapsed,
			wantStatus: pix.StatusPending,
		},
		{
			name:        "expired is idempotent no-op",
			from:        pix.StatusExpired,
			now:         tExpires.Add(time.Hour),
			wantChanged: false,
			wantStatus:  pix.StatusExpired,
		},
		{
			name:        "paid wins over expiry",
			from:        pix.StatusPaid,
			now:         tExpires.Add(time.Hour),
			wantChanged: false,
			wantStatus:  pix.StatusPaid,
		},
		{
			name:        "cancelled is no-op on expire",
			from:        pix.StatusCancelled,
			now:         tExpires.Add(time.Hour),
			wantChanged: false,
			wantStatus:  pix.StatusCancelled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := chargeInStatus(t, tc.from)
			changed, err := c.Expire(tc.now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if c.Status() != tc.wantStatus {
					t.Errorf("status changed on failed transition: got %s, want %s", c.Status(), tc.wantStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if c.Status() != tc.wantStatus {
				t.Errorf("status = %s, want %s", c.Status(), tc.wantStatus)
			}
		})
	}
}

func TestCancel_Transitions(t *testing.T) {
	tests := []struct {
		name        string
		from        pix.Status
		wantChanged bool
		wantErr     error
		wantStatus  pix.Status
	}{
		{name: "pending becomes cancelled", from: pix.StatusPending, wantChanged: true, wantStatus: pix.StatusCancelled},
		{name: "cancelled is idempotent no-op", from: pix.StatusCancelled, wantChanged: false, wantStatus: pix.StatusCancelled},
		{name: "paid rejects cancel", from: pix.StatusPaid, wantErr: pix.ErrInvalidTransition, wantStatus: pix.StatusPaid},
		{name: "expired rejects cancel", from: pix.StatusExpired, wantErr: pix.ErrInvalidTransition, wantStatus: pix.StatusExpired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := chargeInStatus(t, tc.from)
			changed, err := c.Cancel(tNow.Add(time.Hour))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if c.Status() != tc.wantStatus {
					t.Errorf("status changed on failed transition: got %s, want %s", c.Status(), tc.wantStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if c.Status() != tc.wantStatus {
				t.Errorf("status = %s, want %s", c.Status(), tc.wantStatus)
			}
		})
	}
}

// TestStateMachine_CorruptStatusRejected covers the defensive default
// branches: a hydrated row with an unknown status (e.g. a future
// migration adds a value but a stale binary reads it) must surface
// ErrInvalidTransition on Expire rather than panic or silently mutate.
func TestStateMachine_CorruptStatusRejected(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	invoice := uuid.New()
	c := pix.HydrateCharge(
		id, tenant, invoice,
		externalID, qrCode, copyPaste,
		pix.Status("future-state"),
		nil,
		tExpires, tNow, tNow,
	)
	if _, err := c.Expire(tExpires.Add(time.Hour)); !errors.Is(err, pix.ErrInvalidTransition) {
		t.Errorf("Expire on corrupt status: got %v, want ErrInvalidTransition", err)
	}
	if _, err := c.MarkPaid(tNow.Add(time.Hour)); !errors.Is(err, pix.ErrInvalidTransition) {
		t.Errorf("MarkPaid on corrupt status: got %v, want ErrInvalidTransition", err)
	}
	if _, err := c.Cancel(tNow.Add(time.Hour)); !errors.Is(err, pix.ErrInvalidTransition) {
		t.Errorf("Cancel on corrupt status: got %v, want ErrInvalidTransition", err)
	}
}

// chargeInStatus builds a charge in the requested status by walking
// the state machine from a fresh pending charge. Used to keep the
// transition tables above readable without 4× setup helpers.
func chargeInStatus(t *testing.T, s pix.Status) *pix.PIXCharge {
	t.Helper()
	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	switch s {
	case pix.StatusPending:
		return c
	case pix.StatusPaid:
		if _, err := c.MarkPaid(tNow.Add(time.Minute)); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		return c
	case pix.StatusExpired:
		if _, err := c.Expire(tExpires.Add(time.Second)); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		return c
	case pix.StatusCancelled:
		if _, err := c.Cancel(tNow.Add(time.Minute)); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		return c
	default:
		t.Fatalf("unsupported status in chargeInStatus: %s", s)
		return nil
	}
}
