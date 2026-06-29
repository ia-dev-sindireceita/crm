package main

// SIN-66259 — WhatsApp non-official session provisioning UI (Fase 4) wireup.
//
// This composition root binds the HTMX provisioning surface
// (internal/web/wasession) to its two ports:
//
//   - Provisioner → the Fase 1 Manager + the QR cache, via
//     managerProvisioner. The handler shows status / QR and connects /
//     disconnects a tenant session without ever importing whatsmeow.
//   - ConsentGate → the audited internal/iam/consent RecordingRegistry
//     (the same store the cookie-consent banner uses), via
//     consentGateAdapter. A recorded grant is a row + an audit-log entry
//     (who / when / notice-version + IP / UA); the connect path refuses to
//     activate the session without a current-notice grant.
//
// Deny-by-default: the surface mounts ONLY when the session transport is
// mounted (FEATURE_WA_SESSION_ENABLED + the two DSNs) AND the consent
// registry can be built. A nil return leaves every /settings/whatsapp-
// session* route unmounted (chi 404), mirroring the other web/* wires.
//
// Consent purpose: the ban-risk acceptance is recorded under
// consent.PurposeTermsOfService with the namespaced version
// webwasession.NoticeVersion ("wa-session-risk-…"). The version namespace
// isolates it from any generic ToS grant — webwasession.consentCurrent
// requires an exact version match — so the two never satisfy each other's
// gate. (A dedicated consent.Purpose would need a CHECK-constraint
// migration on consent_record; deferred as a possible follow-up.)

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgconsent "github.com/pericles-luz/crm/internal/adapter/db/postgres/consent"
	"github.com/pericles-luz/crm/internal/iam/consent"
	"github.com/pericles-luz/crm/internal/wasession"
	webwasession "github.com/pericles-luz/crm/internal/web/wasession"
)

// qrSink is the narrow write side of the QR cache the inbound pump drives.
// *wasession.QRCache satisfies it; tests inject a recorder.
type qrSink interface {
	Put(tenantID uuid.UUID, qr wasession.QRCode)
	Clear(tenantID uuid.UUID)
}

// sessionController is the narrow slice of the Fase 1 Manager the
// provisioner drives. *wasession.Manager satisfies it.
type sessionController interface {
	StartSession(ctx context.Context, tenantID uuid.UUID) error
	StopSession(tenantID uuid.UUID) error
	Status(tenantID uuid.UUID) (wasession.Status, bool)
}

// qrReader is the read side of the QR cache. *wasession.QRCache satisfies
// it.
type qrReader interface {
	Get(tenantID uuid.UUID) (wasession.QRCode, bool)
}

// managerProvisioner adapts the Manager + QR cache into the
// web/wasession.Provisioner port.
type managerProvisioner struct {
	ctrl sessionController
	qr   qrReader
}

// Snapshot reports the tenant session state. "Active" means a supervised
// session exists; the raw status string and (while pairing) the QR payload
// come from the Manager and the cache respectively. Reads never fail — an
// absent session is simply inactive — so the error is always nil.
func (p managerProvisioner) Snapshot(_ context.Context, tenantID uuid.UUID) (webwasession.SessionSnapshot, error) {
	snap := webwasession.SessionSnapshot{}
	if st, ok := p.ctrl.Status(tenantID); ok {
		snap.Active = true
		snap.Status = st.String()
	}
	if p.qr != nil {
		if qr, ok := p.qr.Get(tenantID); ok {
			snap.QRPayload = qr.Code.Reveal()
		}
	}
	return snap, nil
}

// Connect activates (or reconnects) the tenant session. It uses a
// background context, not the request context: StartSession launches a
// long-lived supervised goroutine, so tying provisioning to the request
// lifecycle would cancel device setup when the HTTP response returns. An
// already-running session is success (idempotent reconnect).
func (p managerProvisioner) Connect(_ context.Context, tenantID uuid.UUID) error {
	err := p.ctrl.StartSession(context.Background(), tenantID)
	if errors.Is(err, wasession.ErrSessionExists) {
		return nil
	}
	return err
}

// Disconnect tears the session down. An absent session is success
// (idempotent).
func (p managerProvisioner) Disconnect(_ context.Context, tenantID uuid.UUID) error {
	err := p.ctrl.StopSession(tenantID)
	if errors.Is(err, wasession.ErrSessionNotFound) {
		return nil
	}
	return err
}

// consentRecorder is the narrow slice of consent.RecordingRegistry the
// adapter needs. *consent.RecordingRegistry satisfies it.
type consentRecorder interface {
	Record(ctx context.Context, rec consent.ConsentRecord) (consent.ConsentRecord, bool, error)
	Latest(ctx context.Context, tenant uuid.UUID, subject consent.Subject, purpose consent.Purpose) (*consent.ConsentRecord, error)
}

// consentGateAdapter binds the web ConsentGate port to the audited consent
// registry, fixing the subject (the operator) and the ToS purpose.
type consentGateAdapter struct {
	reg consentRecorder
}

func (a consentGateAdapter) Latest(ctx context.Context, tenantID, userID uuid.UUID) (webwasession.ConsentState, error) {
	rec, err := a.reg.Latest(ctx, tenantID, subjectForUser(userID), consent.PurposeTermsOfService)
	if err != nil {
		return webwasession.ConsentState{}, err
	}
	if rec == nil {
		return webwasession.ConsentState{}, nil
	}
	return webwasession.ConsentState{
		Granted: rec.Granted && rec.RevokedAt == nil,
		Version: rec.Version,
		At:      rec.GrantedAt,
	}, nil
}

func (a consentGateAdapter) Record(ctx context.Context, in webwasession.ConsentInput) error {
	_, _, err := a.reg.Record(ctx, consent.ConsentRecord{
		TenantID:  in.TenantID,
		Subject:   subjectForUser(in.UserID),
		Purpose:   consent.PurposeTermsOfService,
		Version:   in.Version,
		Granted:   true,
		IP:        in.IP,
		UserAgent: in.UserAgent,
	})
	return err
}

// subjectForUser builds the consent Subject for an operator user id.
func subjectForUser(userID uuid.UUID) consent.Subject {
	return consent.Subject{Type: consent.SubjectUser, ID: userID.String()}
}

// buildWASessionUIHandler assembles the /settings/whatsapp-session HTMX mux.
// Returns (nil, no-op) when the session transport is not mounted (prov nil)
// or the consent registry cannot be built — the surface then stays
// unmounted (deny-by-default).
func buildWASessionUIHandler(ctx context.Context, getenv func(string) string, prov *managerProvisioner) (http.Handler, func()) {
	noop := func() {}
	if prov == nil {
		log.Printf("crm: wa session UI disabled (transport not mounted)")
		return nil, noop
	}
	dsn := getenv(postgresadapter.EnvDSN)
	if dsn == "" {
		log.Printf("crm: wa session UI disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := postgresadapter.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: wa session UI disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgconsent.NewStore(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session UI disabled — consent store: %v", err)
		return nil, noop
	}
	splitLogger, err := postgresadapter.NewSplitAuditLogger(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session UI disabled — audit logger: %v", err)
		return nil, noop
	}
	registry, err := consent.NewRecordingRegistry(store, splitLogger, consent.RecordingConfig{
		Now:              func() time.Time { return time.Now().UTC() },
		ActorFromContext: consentActorFromContext,
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session UI disabled — recording registry: %v", err)
		return nil, noop
	}
	h, err := webwasession.New(webwasession.Deps{
		Provisioner: prov,
		Consent:     consentGateAdapter{reg: registry},
		UserID:      userIDFromSessionContext,
		CSRFToken:   csrfTokenFromSessionContext,
		Logger:      slog.Default(),
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: wa session UI disabled — handler: %v", err)
		return nil, noop
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("crm: wa session UI mounted on %s", webwasession.BasePath)
	return mux, func() { pool.Close() }
}
