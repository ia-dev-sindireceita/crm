// Package usecase holds the application services for the inbox
// aggregate. PR4 ships ReceiveInbound (the carrier-adapter entry
// point) and SendOutbound (the HTMX-handler entry point). The full
// dependency map:
//
//	ReceiveInbound  ← InboundChannel adapter (PR6/7/8)
//	  ├─ InboundDedupRepository.Claim/MarkProcessed       (this PR)
//	  ├─ contacts.UpsertContactByChannel                  (PR3)
//	  └─ Repository.FindOpenConversation/CreateConversation/SaveMessage (this PR)
//
//	SendOutbound    ← HTMX outbound handler (PR7+)
//	  ├─ Repository.GetConversation/SaveMessage/UpdateMessage (this PR)
//	  ├─ WalletDebitor.Debit                              (PR5 implementation)
//	  └─ OutboundChannel.SendMessage                      (PR8)
//
// Use-cases depend on ports; they never import database drivers or
// vendor SDKs. Wiring (which implementation is plugged in) is done at
// the composition root in cmd/server.
package usecase

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/media/scanner"
)

// TenantLeadPolicy is the slim port the F2-07.2 auto-attribution path
// reads. Production wiring binds it to the postgres TenantResolver's
// DefaultLeadUserID method, which serves `SELECT default_lead_user_id
// FROM tenants WHERE id = $1`. Returns:
//
//   - (&user, nil)  : the tenant has a default leader configured
//   - (nil,   nil)  : the tenant exists but has no default leader
//     (UI shows "sem líder")
//   - (nil,   err)  : tenant missing, lookup transient error, etc.
type TenantLeadPolicy interface {
	DefaultLeadUserID(ctx context.Context, tenantID uuid.UUID) (*uuid.UUID, error)
}

// ContactUpserter is the slim subset of contacts/usecase the
// receive-inbound flow needs. Decoupling on the method signature lets
// tests inject a fake contact resolver without spinning up Postgres.
// The production wiring binds it to *contactsusecase.UpsertContactByChannel.
type ContactUpserter interface {
	Execute(ctx context.Context, in contactsusecase.Input) (contactsusecase.Result, error)
}

// ReceiveInbound orchestrates a webhook-style inbound delivery:
//
//  1. Claim the (channel, channel_external_id) pair on the global dedup
//     ledger. If already claimed → return nil (the carrier is retrying;
//     we MUST NOT double-emit a message).
//  2. Resolve / create the Contact via UpsertContactByChannel.
//  3. Find an open Conversation for (tenant, contact, channel) or
//     create one.
//  4. Save the Message + bump LastMessageAt on the conversation.
//  5. MarkProcessed on the dedup ledger.
//
// Idempotency contract: calling Execute twice for the same
// (channel, channel_external_id) MUST result in exactly one persisted
// message and exactly one persisted contact (AC #4).
type ReceiveInbound struct {
	repo        inbox.Repository
	dedup       inbox.InboundDedupRepository
	contacts    ContactUpserter
	leadPolicy  TenantLeadPolicy
	assignments inbox.AssignmentRepository
	// campaignLinker is the SIN-62959 attribution port — wired by the
	// composition root via SetCampaignLinker after construction so the
	// existing NewReceiveInbound / NewReceiveInboundWithLeadership APIs
	// stay backwards-compatible. Nil disables the attribution hook
	// (see linkContactToCampaign for the soft-fail contract).
	campaignLinker CampaignLinker
	// campaignLogger receives the InfoContext / WarnContext entries
	// emitted by the attribution hook. The wire injects the process
	// logger via SetCampaignLinkerLogger; nil falls back to slog.Default.
	campaignLogger *slog.Logger
	// campaignMarkerKey is the HMAC secret used to verify the signed
	// attribution marker (SIN-62982). The composition root wires it
	// via SetCampaignMarkerKey from CAMPAIGNS_MARKER_SIGNING_KEY; the
	// zero MarkerKey disables verification so legacy unsigned markers
	// continue to link if campaignMarkerAllowLegacy is true.
	campaignMarkerKey campaigns.MarkerKey
	// campaignMarkerAllowLegacy controls whether the hook accepts the
	// pre-SIN-62982 unsigned marker form. Left true for the 90-day
	// cookie-TTL transition window; a follow-up flips it false.
	campaignMarkerAllowLegacy bool
	// inboundPublisher is the optional NATS outbound hook (SIN-62960).
	// Wired by the composition root via SetInboundMessagePublisher so
	// the existing NewReceiveInbound / NewReceiveInboundWithLeadership
	// APIs stay backwards-compatible. Nil disables fan-out (see
	// publishInboundMessage for the soft-fail contract).
	inboundPublisher       InboundMessagePublisher
	inboundPublisherLogger *slog.Logger
}

// NewReceiveInbound wires the use case to its dependencies. nil port
// arguments are programming errors caught at construction so the
// process crashes before serving the first request.
//
// Built without the F2-07.2 leadership path: conversations created
// through this constructor stay unassigned. The composition root MUST
// use NewReceiveInboundWithLeadership so tenant.default_lead_user_id
// is honoured.
func NewReceiveInbound(repo inbox.Repository, dedup inbox.InboundDedupRepository, c ContactUpserter) (*ReceiveInbound, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	if dedup == nil {
		return nil, errors.New("inbox/usecase: dedup must not be nil")
	}
	if c == nil {
		return nil, errors.New("inbox/usecase: contacts upserter must not be nil")
	}
	return &ReceiveInbound{
		repo:     repo,
		dedup:    dedup,
		contacts: c,
		// SIN-62982 compat-window default: accept legacy unsigned
		// markers so messages sent before the HMAC rollout still link.
		// A follow-up will flip this to false once the 90-day cookie
		// TTL has elapsed; production wiring may override via
		// SetCampaignMarkerAllowLegacy.
		campaignMarkerAllowLegacy: true,
	}, nil
}

// MustNewReceiveInbound is the panic-on-error variant for the
// composition root.
func MustNewReceiveInbound(repo inbox.Repository, dedup inbox.InboundDedupRepository, c ContactUpserter) *ReceiveInbound {
	u, err := NewReceiveInbound(repo, dedup, c)
	if err != nil {
		panic(err)
	}
	return u
}

// NewReceiveInboundWithLeadership is the production constructor (F2-07.2,
// SIN-62833) that wires the auto-attribution policy. After a conversation
// is freshly created, Execute consults leadPolicy.DefaultLeadUserID for the
// event's TenantID and — when default_lead_user_id is set — appends an
// assignment_history row with reason='lead' so the conversation lands on
// the configured operator. When default_lead_user_id is nil the path is a
// no-op (the legacy "sem líder" UI state is preserved).
//
// nil leadPolicy/assignments arguments are programming errors. Callers that
// genuinely want no leadership policy (mostly tests) must use
// NewReceiveInbound instead.
func NewReceiveInboundWithLeadership(
	repo inbox.Repository,
	dedup inbox.InboundDedupRepository,
	c ContactUpserter,
	leadPolicy TenantLeadPolicy,
	assignments inbox.AssignmentRepository,
) (*ReceiveInbound, error) {
	u, err := NewReceiveInbound(repo, dedup, c)
	if err != nil {
		return nil, err
	}
	if leadPolicy == nil {
		return nil, errors.New("inbox/usecase: tenant lead policy must not be nil")
	}
	if assignments == nil {
		return nil, errors.New("inbox/usecase: assignments repo must not be nil")
	}
	u.leadPolicy = leadPolicy
	u.assignments = assignments
	return u, nil
}

// MustNewReceiveInboundWithLeadership is the panic-on-error variant for
// the composition root.
func MustNewReceiveInboundWithLeadership(
	repo inbox.Repository,
	dedup inbox.InboundDedupRepository,
	c ContactUpserter,
	leadPolicy TenantLeadPolicy,
	assignments inbox.AssignmentRepository,
) *ReceiveInbound {
	u, err := NewReceiveInboundWithLeadership(repo, dedup, c, leadPolicy, assignments)
	if err != nil {
		panic(err)
	}
	return u
}

// ReceiveInboundResult reports the outcome of an inbound delivery. The
// boolean Duplicate is true when dedup rejected the event — useful for
// metrics and for the carrier adapter to choose the right HTTP ack.
type ReceiveInboundResult struct {
	Conversation *inbox.Conversation
	Message      *inbox.Message
	Contact      *contacts.Contact
	Duplicate    bool
}

// HandleInbound implements the inbox.InboundChannel port. Carrier
// adapters (PR6 WhatsApp webhook receiver, PR7+ webchat) call this
// instead of Execute when they don't need the rich ReceiveInboundResult
// — they care only about success / duplicate / error, and duplicate is
// already encoded as a nil error (ADR 0087 §D3 step 4: the worker
// commits and ACKs the carrier with no Message created).
func (u *ReceiveInbound) HandleInbound(ctx context.Context, ev inbox.InboundEvent) error {
	_, err := u.Execute(ctx, ev)
	return err
}

// MaterialiseInbound implements inbox.InboundMessageMaterialiser. It
// runs the same dedup/contact/save pipeline as Execute and surfaces
// the persisted message id so adapters that fan out per-message work
// (e.g. messenger.requestMediaScans → MediaScanPublisher.PublishScanRequest)
// can correlate `message.media` rows with the NATS envelope. On a
// duplicate redelivery the result carries Duplicate=true and a zero
// MessageID — callers MUST treat that as "previous delivery already
// fanned out" and skip republishing (SIN-62848 AC #4).
func (u *ReceiveInbound) MaterialiseInbound(ctx context.Context, ev inbox.InboundEvent) (inbox.MaterialisedInbound, error) {
	res, err := u.Execute(ctx, ev)
	if err != nil {
		return inbox.MaterialisedInbound{}, err
	}
	if res.Duplicate {
		return inbox.MaterialisedInbound{Duplicate: true}, nil
	}
	return inbox.MaterialisedInbound{MessageID: res.Message.ID}, nil
}

// Compile-time guards: ReceiveInbound satisfies both inbox-side ports
// so the composition root can hand it to either a thin
// (HandleInbound-only) or rich (MaterialiseInbound) adapter without
// re-wiring.
var (
	_ inbox.InboundChannel             = (*ReceiveInbound)(nil)
	_ inbox.InboundMessageMaterialiser = (*ReceiveInbound)(nil)
)

// Execute runs the inbound pipeline. See type-level doc-comment for
// the full algorithm.
func (u *ReceiveInbound) Execute(ctx context.Context, ev inbox.InboundEvent) (ReceiveInboundResult, error) {
	if ev.TenantID == uuid.Nil {
		return ReceiveInboundResult{}, inbox.ErrInvalidTenant
	}
	channel := strings.ToLower(strings.TrimSpace(ev.Channel))
	if channel == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidChannel
	}
	externalID := strings.TrimSpace(ev.ChannelExternalID)
	if externalID == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidStatus
	}
	if strings.TrimSpace(ev.SenderExternalID) == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidChannel
	}

	// 1. Dedup claim.
	if err := u.dedup.Claim(ctx, channel, externalID); err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			return ReceiveInboundResult{Duplicate: true}, nil
		}
		return ReceiveInboundResult{}, err
	}

	// 2. Resolve / create the contact.
	res, err := u.contacts.Execute(ctx, contactsusecase.Input{
		TenantID:    ev.TenantID,
		Channel:     channel,
		ExternalID:  ev.SenderExternalID,
		DisplayName: fallbackDisplay(ev.SenderDisplayName, ev.SenderExternalID),
	})
	if err != nil {
		return ReceiveInboundResult{}, err
	}

	// 3. Find an open conversation or create one.
	conv, err := u.repo.FindOpenConversation(ctx, ev.TenantID, res.Contact.ID, channel)
	if err != nil && !errors.Is(err, inbox.ErrNotFound) {
		return ReceiveInboundResult{}, err
	}
	if errors.Is(err, inbox.ErrNotFound) {
		conv, err = inbox.NewConversation(ev.TenantID, res.Contact.ID, channel)
		if err != nil {
			return ReceiveInboundResult{}, err
		}
		if err := u.repo.CreateConversation(ctx, conv); err != nil {
			return ReceiveInboundResult{}, err
		}
		// F2-07.2 (SIN-62833): apply tenant.default_lead_user_id as the
		// initial leadership when configured. Only runs when the leadership
		// constructor was used; legacy NewReceiveInbound callers leave the
		// conversation unassigned (UI shows "sem líder").
		if err := u.attributeInitialLead(ctx, conv); err != nil {
			return ReceiveInboundResult{}, err
		}
	}

	// 4. Persist the message and bump LastMessageAt.
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:          ev.TenantID,
		ConversationID:    conv.ID,
		Direction:         inbox.MessageDirectionIn,
		Body:              ev.Body,
		ChannelExternalID: externalID,
	})
	if err != nil {
		return ReceiveInboundResult{}, err
	}
	if !ev.OccurredAt.IsZero() {
		m.CreatedAt = ev.OccurredAt
	}
	// SIN-62848 AC #1: messages with attachments materialise with
	// scan_status="pending" so the inbox view never renders an
	// unscanned blob. Hash/Format stay empty until the MediaScanner
	// worker patches the row to "clean" (post-clean re-materialisation
	// owns Hash/Format wiring); the views projector already drops the
	// hash for any non-clean status, so a pending row UI-renders as a
	// "scanning" placeholder rather than a deep-link.
	if ev.HasAttachments {
		m.AttachMedia("", "", string(scanner.StatusPending))
	}
	if err := conv.RecordMessage(m); err != nil {
		return ReceiveInboundResult{}, err
	}
	if err := u.repo.SaveMessage(ctx, m); err != nil {
		return ReceiveInboundResult{}, err
	}

	// 5. Close the dedup row.
	if err := u.dedup.MarkProcessed(ctx, channel, externalID); err != nil {
		return ReceiveInboundResult{}, err
	}

	// 6. Attribution hook (SIN-62959 AC #3 — soft-fail). The hook
	//    scans the just-persisted body for a [crm:<click_id>] marker
	//    that the public redirect handler embedded via the
	//    {click_id} placeholder in the campaign's redirect_url. A
	//    miss / absence / linker error never aborts the inbound
	//    delivery — see linkContactToCampaign for the soft-fail
	//    contract. This is the only place ReceiveInbound talks to the
	//    campaigns boundary.
	logger := u.campaignLogger
	if logger == nil {
		logger = slog.Default()
	}
	u.linkContactToCampaign(ctx, logger, ev.TenantID, res.Contact.ID, m.Body)

	// 7. Funnel-engine fan-out (SIN-62960 — soft-fail). Publishes the
	//    persisted message on the JetStream inbound subject so the
	//    funnel rule engine (a separate worker process) can evaluate
	//    rules and apply actions. Disabled when no publisher is wired;
	//    publish errors degrade gracefully — the inbox row is the
	//    source of truth, the bus is a notification.
	publisherLogger := u.inboundPublisherLogger
	if publisherLogger == nil {
		publisherLogger = slog.Default()
	}
	u.publishInboundMessage(ctx, publisherLogger, PublishedInboundMessage{
		TenantID:       ev.TenantID,
		ConversationID: conv.ID,
		MessageID:      m.ID,
		Channel:        channel,
		Body:           m.Body,
		OccurredAt:     m.CreatedAt,
	})

	return ReceiveInboundResult{
		Conversation: conv,
		Message:      m,
		Contact:      res.Contact,
		Duplicate:    false,
	}, nil
}

// attributeInitialLead implements the F2-07.2 auto-attribution policy.
// When leadership ports are wired and tenant.default_lead_user_id is
// populated, it appends an assignment_history row (reason='lead') for
// the freshly-created Conversation and refreshes the in-memory
// aggregate via Conversation.SetHistory.
//
// When the leadership ports are nil (NewReceiveInbound path) the call
// is a no-op. A nil DefaultLeadUserID is also a no-op — the
// conversation legitimately has no default lead and the UI surfaces it
// as "sem líder". Policy-lookup errors are surfaced to the caller so a
// transient DB hiccup does not silently land the conversation
// unassigned; ErrTenantNotFound at this point would mean the tenant
// row that authenticated the inbound event has since vanished, which
// is a system-integrity bug worth failing loudly on.
func (u *ReceiveInbound) attributeInitialLead(ctx context.Context, conv *inbox.Conversation) error {
	if u.leadPolicy == nil || u.assignments == nil {
		return nil
	}
	leadUserID, err := u.leadPolicy.DefaultLeadUserID(ctx, conv.TenantID)
	if err != nil {
		return err
	}
	if leadUserID == nil {
		return nil
	}
	a, err := u.assignments.AppendHistory(ctx, conv.TenantID, conv.ID, *leadUserID, inbox.LeadReasonLead)
	if err != nil {
		return err
	}
	// Hydrate the in-memory aggregate with the exact row the adapter
	// persisted so callers downstream observe the same id/AssignedAt
	// the database wrote. SetHistory also refreshes AssignedUserID for
	// legacy callers that read the denormalised field.
	conv.SetHistory([]*inbox.Assignment{a})
	return nil
}

// fallbackDisplay picks a display name when the carrier did not send
// a profile name. Execute already rejects events with a blank
// SenderExternalID, so sender is always non-empty when this is called
// — the phone number is a reasonable last resort.
func fallbackDisplay(profile, sender string) string {
	if s := strings.TrimSpace(profile); s != "" {
		return s
	}
	return strings.TrimSpace(sender)
}
