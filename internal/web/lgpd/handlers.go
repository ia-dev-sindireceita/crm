package lgpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	domainaudit "github.com/pericles-luz/crm/internal/iam/audit"
	domain "github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// AuditWriter is the narrow slice of iam/audit.SplitLogger the handler
// uses. Defined locally so tests can supply a stub that records the
// event payload instead of round-tripping to Postgres.
type AuditWriter interface {
	WriteData(ctx context.Context, event domainaudit.DataAuditEvent) error
}

// Deps bundles the collaborators the handler depends on. All ports
// except Now and Logger are required; Now and Logger default sensibly
// so tests can leave them zero.
type Deps struct {
	// Export is the read port for the contact export.
	Export domain.ExportRepository
	// Deletions is the write port for the deletion-request tombstone.
	Deletions domain.DeletionRepository
	// Audit writes one row per request so the AC #6 audit obligation
	// is satisfied at the same call site as the user-facing action.
	Audit AuditWriter
	// Policy supplies the fiscal retention window.
	Policy domain.RetentionPolicy
	// Now returns the current time. Defaults to time.Now().UTC.
	Now func() time.Time
	// Logger is the structured logger. Defaults to slog.Default.
	Logger *slog.Logger
}

// Handler serves /admin/lgpd/export and /admin/lgpd/delete. Construct
// once with New; the returned Handler is safe to share across
// goroutines.
type Handler struct {
	deps Deps
}

// New validates deps and returns a ready Handler. Required: Export,
// Deletions, Audit. Policy with FiscalYears == 0 falls back to the
// LGPD default fiscal retention window.
func New(deps Deps) (*Handler, error) {
	if deps.Export == nil {
		return nil, errors.New("web/lgpd: Export is required")
	}
	if deps.Deletions == nil {
		return nil, errors.New("web/lgpd: Deletions is required")
	}
	if deps.Audit == nil {
		return nil, errors.New("web/lgpd: Audit is required")
	}
	if deps.Policy.FiscalYears == 0 {
		deps.Policy = domain.RetentionPolicy{FiscalYears: domain.DefaultFiscalRetentionYears}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts both endpoints on mux. Go 1.22 method+pattern syntax
// is the same shape every other web/* package uses.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/lgpd/export", h.export)
	mux.HandleFunc("POST /admin/lgpd/delete", h.delete)
}

// Export streams the per-contact ZIP. Exported for tests that drive
// the handler directly without going through the mux.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) { h.export(w, r) }

// Delete persists a new (or refreshed) deletion request. Exported for
// tests that drive the handler directly without going through the mux.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) { h.delete(w, r) }

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	principal, _ := iam.PrincipalFromContext(r.Context())
	contactID, err := parseContactQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bundle, err := h.buildBundle(r.Context(), tenant.ID, contactID)
	if err != nil {
		if errors.Is(err, domain.ErrDeletionRequestNotFound) {
			http.Error(w, "contact not found", http.StatusNotFound)
			return
		}
		h.deps.Logger.Error("lgpd export: build bundle", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.audit(r.Context(), domainaudit.DataEventLGPDExport, principal, tenant.ID, contactID, clientIP(r)); err != nil {
		// Non-repudiation: AC #6 requires the audit row before we
		// release the data. Returning 500 keeps the contract honest.
		h.deps.Logger.Error("lgpd export: audit write", "err", err.Error())
		http.Error(w, "audit failed", http.StatusInternalServerError)
		return
	}
	filename := fmt.Sprintf("lgpd-export-%s-%s.zip", contactID, h.deps.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := domain.WriteBundle(w, bundle); err != nil {
		// The header is already on the wire; log and abandon — the
		// client sees a truncated ZIP, which is a recoverable retry
		// from their side.
		h.deps.Logger.Error("lgpd export: zip stream", "err", err.Error())
	}
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	principal, _ := iam.PrincipalFromContext(r.Context())
	req, err := parseDeleteBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := h.deps.Now()
	dr := domain.DeletionRequest{
		TenantID:          tenant.ID,
		ContactID:         req.ContactID,
		RequestedByUserID: principal.UserID,
		Justification:     req.Justification,
		Status:            domain.DeletionStatusPending,
		RetentionUntil:    h.deps.Policy.RetentionUntil(now),
		CreatedAt:         now,
	}
	out, err := h.deps.Deletions.Upsert(r.Context(), dr)
	if err != nil {
		h.deps.Logger.Error("lgpd delete: upsert", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.audit(r.Context(), domainaudit.DataEventLGPDForget, principal, tenant.ID, req.ContactID, clientIP(r)); err != nil {
		h.deps.Logger.Error("lgpd delete: audit write", "err", err.Error())
		http.Error(w, "audit failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, deleteResponse{
		ID:             out.ID.String(),
		Status:         string(out.Status),
		RetentionUntil: out.RetentionUntil.UTC().Format(time.RFC3339),
		ContactID:      out.ContactID.String(),
	})
}

func (h *Handler) buildBundle(ctx context.Context, tenantID, contactID uuid.UUID) (domain.ExportBundle, error) {
	bundle := domain.ExportBundle{GeneratedAt: h.deps.Now().UTC()}
	contact, err := h.deps.Export.GetContact(ctx, tenantID, contactID)
	if err != nil {
		return bundle, fmt.Errorf("contact: %w", err)
	}
	bundle.Contact = contact

	idents, err := h.deps.Export.ListIdentities(ctx, tenantID, contactID)
	if err != nil {
		return bundle, fmt.Errorf("identities: %w", err)
	}
	bundle.Identities = idents

	convs, err := h.deps.Export.ListConversations(ctx, tenantID, contactID)
	if err != nil {
		return bundle, fmt.Errorf("conversations: %w", err)
	}
	bundle.Conversations = convs

	msgs, err := h.deps.Export.ListMessages(ctx, tenantID, contactID)
	if err != nil {
		return bundle, fmt.Errorf("messages: %w", err)
	}
	bundle.Messages = msgs

	billing, err := h.deps.Export.ListBillingEvents(ctx, tenantID, contactID)
	if err != nil {
		return bundle, fmt.Errorf("billing: %w", err)
	}
	bundle.BillingEvents = billing

	consents, err := h.deps.Export.ListConsents(ctx, tenantID)
	if err != nil {
		return bundle, fmt.Errorf("consents: %w", err)
	}
	bundle.Consents = consents

	return bundle, nil
}

func (h *Handler) audit(ctx context.Context, event domainaudit.DataEvent, principal iam.Principal, tenantID, contactID uuid.UUID, ip string) error {
	target := map[string]any{
		"contact_id": contactID.String(),
		"actor_ip":   ip,
	}
	return h.deps.Audit.WriteData(ctx, domainaudit.DataAuditEvent{
		Event:       event,
		ActorUserID: principal.UserID,
		TenantID:    tenantID,
		Target:      target,
		OccurredAt:  h.deps.Now().UTC(),
	})
}

// parseContactQuery enforces a non-zero uuid on ?contact_id=.
func parseContactQuery(r *http.Request) (uuid.UUID, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("contact_id"))
	if raw == "" {
		return uuid.Nil, errors.New("contact_id is required")
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errors.New("contact_id must be a uuid")
	}
	return id, nil
}

type deleteBody struct {
	ContactID     uuid.UUID `json:"contact_id"`
	Justification string    `json:"justification"`
}

// parseDeleteBody validates the JSON request body. Empty fields and
// the zero uuid are rejected at the boundary so the use case never
// has to defend against them.
func parseDeleteBody(r *http.Request) (deleteBody, error) {
	var body deleteBody
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, fmt.Errorf("invalid json body: %v", err)
	}
	if body.ContactID == uuid.Nil {
		return body, errors.New("contact_id is required")
	}
	body.Justification = strings.TrimSpace(body.Justification)
	if body.Justification == "" {
		return body, errors.New("justification is required")
	}
	if len(body.Justification) > 4096 {
		return body, errors.New("justification too long")
	}
	return body, nil
}

type deleteResponse struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	RetentionUntil string `json:"retention_until"`
	ContactID      string `json:"contact_id"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// clientIP strips the port from RemoteAddr. The trusted_realip
// middleware already rewrote RemoteAddr from the upstream proxy
// header for trusted ingress IPs (SIN-62978); we only render the
// final value.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
