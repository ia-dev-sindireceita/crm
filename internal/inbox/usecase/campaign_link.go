package usecase

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
)

// CampaignLinker is the slim port that links a freshly-arrived inbound
// message to the click row that introduced the contact (SIN-62959 AC #3,
// SIN-62197 AC #2). Production wiring binds it to
// *postgres/campaigns.Store.LinkContactToCampaign; tests use
// *campaigns.InMemoryRepository.
//
// The port is intentionally one method — the use case never needs to
// list, query or mutate campaigns from the inbound path. A broader
// surface would invite the wrong dependencies into ReceiveInbound.
type CampaignLinker interface {
	LinkContactToCampaign(ctx context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error
}

// ExtractClickID returns the first attribution token found in body, or
// the empty string when no marker is present. Kept as a thin wrapper
// around campaigns.ExtractClickMarker so pre-SIN-62982 callers that
// only need the click_id half remain source-compatible.
//
// The marker format the redirect handler embeds in the campaign's
// redirect_url via the {click_id} placeholder is documented at
// internal/campaigns.BuildClickToken; both legacy `[crm:<id>]` and
// signed `[crm:<id>.<hmac8>]` forms parse here.
func ExtractClickID(body string) string {
	return campaigns.ExtractClickMarker(body).ClickID
}

// SetCampaignLinker wires the optional CampaignLinker dependency.
// Calling with nil is a no-op so the wire can pass a nil through when
// the campaigns adapter is disabled (DATABASE_URL unset, etc.). The
// linker is consulted lazily on each Execute call so re-wiring at
// runtime is also safe.
//
// The receive-inbound use case stores the dependency on the same
// struct as the leadership policy; using a setter rather than another
// NewReceiveInboundWith… constructor keeps the existing API stable.
func (u *ReceiveInbound) SetCampaignLinker(linker CampaignLinker) {
	u.campaignLinker = linker
}

// SetCampaignLinkerLogger injects the logger the attribution hook uses
// for InfoContext / WarnContext entries. Calling with nil falls back
// to slog.Default at hook time. Production wiring passes the process
// logger; tests pass a discard logger to keep test output clean.
func (u *ReceiveInbound) SetCampaignLinkerLogger(l *slog.Logger) {
	u.campaignLogger = l
}

// SetCampaignMarkerKey wires the HMAC secret the attribution hook uses
// to verify the signed marker (SIN-62982). The zero MarkerKey disables
// verification — the hook then accepts legacy unsigned markers
// (`[crm:<click_id>]`) only, which is the pre-SIN-62982 behaviour.
//
// Production wiring loads the key from CAMPAIGNS_MARKER_SIGNING_KEY at
// composition root and passes it both here and to the redirect handler
// so the two paths agree on the signing key.
func (u *ReceiveInbound) SetCampaignMarkerKey(key campaigns.MarkerKey) {
	u.campaignMarkerKey = key
}

// SetCampaignMarkerAllowLegacy controls whether the hook accepts the
// legacy unsigned marker `[crm:<click_id>]` (no `.<hmac8>` suffix).
// Production wiring leaves this true for the 90-day cookie-TTL
// transition window so markers minted before the SIN-62982 rollout
// remain linkable; a follow-up flips it false once the window has
// elapsed to retire the legacy form.
func (u *ReceiveInbound) SetCampaignMarkerAllowLegacy(allow bool) {
	u.campaignMarkerAllowLegacy = allow
}

// linkContactToCampaign runs the AC #3 attribution hook. It is called
// from Execute after the message has been persisted (so a partial
// failure here does not lose the message — the click ledger is the
// attribution side-effect, the inbox is the source of truth). The
// caller passes the just-persisted body so callers that mutate the
// message body (none today) still see the right tokens.
//
// Outcomes (all soft-fail — never abort the inbound delivery):
//
//   - linker nil → skip silently. The wire chose not to wire the
//     attribution path (DATABASE_URL unset / campaigns adapter
//     disabled).
//   - body has no marker → skip silently. Most inbound messages are
//     organic conversation, not campaign acknowledgements.
//   - marker HMAC missing/invalid (SIN-62982) → log info and skip. A
//     sender forging `[crm:<id>]` without a valid signature MUST NOT
//     produce an attribution row; the rate of these is interesting
//     for an operator dashboard but they do not signal an integrity
//     bug in the system.
//   - linker returns campaigns.ErrNotFound → log info and skip. The
//     marker was a stale token (the click row aged out or the wire
//     pointed at a fresh tenant); not an integrity bug.
//   - linker returns any other error → log warn and skip. The
//     attribution row is not critical-path for the user-facing inbox.
//
// Returns nil unconditionally so Execute can ignore the return value
// without an `_ =` ceremony.
func (u *ReceiveInbound) linkContactToCampaign(ctx context.Context, logger *slog.Logger, tenantID, contactID uuid.UUID, body string) {
	if u.campaignLinker == nil {
		return
	}
	marker := campaigns.ExtractClickMarker(body)
	if !marker.Found {
		return
	}
	if !campaigns.VerifyClickToken(u.campaignMarkerKey, u.campaignMarkerAllowLegacy, tenantID, marker.ClickID, marker.HMACHex) {
		// Forged or unsigned-after-cutover marker. Soft-fail with
		// info-level so the inbound delivery is not blocked and the
		// dashboard can count these without paging the on-call.
		signed := marker.HMACHex != ""
		logger.InfoContext(ctx, "inbox: campaign marker rejected",
			slog.String("tenant_id", tenantID.String()),
			slog.String("click_id", marker.ClickID),
			slog.Bool("signed", signed),
		)
		return
	}
	err := u.campaignLinker.LinkContactToCampaign(ctx, tenantID, marker.ClickID, contactID)
	if err == nil {
		return
	}
	if errors.Is(err, campaigns.ErrNotFound) {
		logger.InfoContext(ctx, "inbox: campaign link miss",
			slog.String("tenant_id", tenantID.String()),
			slog.String("click_id", marker.ClickID),
		)
		return
	}
	logger.WarnContext(ctx, "inbox: campaign link failed",
		slog.String("tenant_id", tenantID.String()),
		slog.String("click_id", marker.ClickID),
		slog.String("err", err.Error()),
	)
}
