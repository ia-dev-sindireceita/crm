package usecase

import (
	"context"
	"errors"
	"log/slog"
	"regexp"

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

// clickMarkerRE matches the documented attribution marker the public
// redirect handler embeds in the campaign's redirect_url via the
// {click_id} placeholder substitution. The pre-filled message body
// looks like:
//
//	"Olá, vim do site [crm:c0a5e0b7-…]"
//
// The handler-side click_id is a uuid v4 today (uuid.NewString), so
// the alphabet is hex + hyphen. The regex is deliberately tight so we
// do not match arbitrary text that happens to start with "[crm:" — the
// goal is "if the marker is exactly the format we generated, link;
// otherwise leave the message untouched".
//
// Anchored at the bracket pair: no whitespace allowed inside; one
// match per body is enough (a redirect handler emits one click_id per
// click). FindStringSubmatch returns the captured group on a match.
var clickMarkerRE = regexp.MustCompile(`\[crm:([A-Za-z0-9-]{8,128})\]`)

// ExtractClickID returns the first attribution token found in body, or
// the empty string when no marker is present. Tests and the inbound
// hook share this helper so the parsing rule lives in one place.
func ExtractClickID(body string) string {
	m := clickMarkerRE.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
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
//   - linker returns campaigns.ErrNotFound → log info and skip. The
//     marker was a forged or stale token; not an integrity bug.
//   - linker returns any other error → log warn and skip. The
//     attribution row is not critical-path for the user-facing inbox.
//
// Returns nil unconditionally so Execute can ignore the return value
// without an `_ =` ceremony.
func (u *ReceiveInbound) linkContactToCampaign(ctx context.Context, logger *slog.Logger, tenantID, contactID uuid.UUID, body string) {
	if u.campaignLinker == nil {
		return
	}
	clickID := ExtractClickID(body)
	if clickID == "" {
		return
	}
	err := u.campaignLinker.LinkContactToCampaign(ctx, tenantID, clickID, contactID)
	if err == nil {
		return
	}
	if errors.Is(err, campaigns.ErrNotFound) {
		logger.InfoContext(ctx, "inbox: campaign link miss",
			slog.String("tenant_id", tenantID.String()),
			slog.String("click_id", clickID),
		)
		return
	}
	logger.WarnContext(ctx, "inbox: campaign link failed",
		slog.String("tenant_id", tenantID.String()),
		slog.String("click_id", clickID),
		slog.String("err", err.Error()),
	)
}
