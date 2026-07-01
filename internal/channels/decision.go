package channels

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Decide is the pure per-resource channel access rule — the domain half
// of the two-layer defense-in-depth documented in ADR-0109. The *surface*
// role gate (iam.RBACAuthorizer, ADR-0090) already decided the caller may
// touch the inbox at all; Decide answers the narrower per-resource
// question "may this caller act on THIS channel instance":
//
//   - a gerente always may (role override — a manager owns every channel
//     in their tenant);
//   - an open (non-restricted) channel is visible to every atendente of
//     the tenant, matching today's zero-regression backfill posture;
//   - a restricted channel is limited to the users holding an explicit
//     channel_access grant.
//
// Keeping the rule a pure, dependency-free function makes it exhaustively
// table-testable without a database and gives both the maintenance UI and
// the P4 inbox read path a single source of truth. The DB-backed
// composition (resolve the channel's Restricted flag + look up the grant)
// lives in AccessService below; Decide is the decision it applies.
func Decide(isGerente, restricted, hasGrant bool) bool {
	if isGerente {
		return true
	}
	if !restricted {
		return true
	}
	return hasGrant
}

// ErrNilRepository / ErrNilAccessPolicy are returned by NewAccessService
// when a required collaborator is missing, so a wiring mistake fails
// loudly at construction rather than nil-panicking at first request.
var (
	ErrNilRepository   = errors.New("channels: AccessService requires a Repository")
	ErrNilAccessPolicy = errors.New("channels: AccessService requires a ChannelAccessPolicy")
)

// AccessService composes the channel Repository (which owns the
// Restricted flag) with the raw ChannelAccessPolicy grant store to answer
// the *effective* access questions the application enforces. It is the
// reusable enforcement primitive for the per-resource channel gate: the
// P3 maintenance surface and the P4 inbox read path both call it so the
// rule cannot drift between "what the UI shows" and "what the server
// allows".
//
// The caller supplies isGerente rather than the service resolving the
// role itself: the role lives in internal/iam (the surface layer), and
// threading it in as a bool keeps this domain package free of an iam
// import (accept-broad, no dependency cycle). Every method is
// tenant-scoped and delegates the actual reads to the RLS-scoped ports.
type AccessService struct {
	repo   Repository
	grants ChannelAccessPolicy
}

// NewAccessService validates and wires the service. Both collaborators
// are required.
func NewAccessService(repo Repository, grants ChannelAccessPolicy) (*AccessService, error) {
	if repo == nil {
		return nil, ErrNilRepository
	}
	if grants == nil {
		return nil, ErrNilAccessPolicy
	}
	return &AccessService{repo: repo, grants: grants}, nil
}

// CanAccessChannel reports whether userID may act on channelID within
// tenantID, applying Decide on top of the channel's Restricted flag and
// (for a restricted channel) the explicit grant.
//
// A non-existent or RLS-hidden channel yields (false, nil) — even for a
// gerente. The gerente override grants access to the tenant's real
// channels, not to a phantom id an adversary might probe; collapsing the
// unknown-channel case to a plain deny also means the caller cannot
// distinguish "exists but you lack access" from "does not exist". The
// grant lookup is skipped entirely for gerentes and for open channels, so
// the common path costs a single Repository.Get.
func (s *AccessService) CanAccessChannel(ctx context.Context, tenantID, userID, channelID uuid.UUID) (bool, error) {
	return s.canAccess(ctx, tenantID, userID, channelID, false)
}

// CanAccessChannelAsGerente is CanAccessChannel with the gerente role
// override applied: the caller has already been resolved as a gerente of
// tenantID by the surface layer. It still requires the channel to exist
// under the tenant scope.
func (s *AccessService) CanAccessChannelAsGerente(ctx context.Context, tenantID, channelID uuid.UUID) (bool, error) {
	return s.canAccess(ctx, tenantID, uuid.Nil, channelID, true)
}

func (s *AccessService) canAccess(ctx context.Context, tenantID, userID, channelID uuid.UUID, isGerente bool) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("channels: CanAccessChannel: tenant id is nil")
	}
	ch, err := s.repo.Get(ctx, tenantID, channelID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if isGerente {
		return true, nil
	}
	if !ch.Restricted {
		return true, nil
	}
	granted, err := s.grants.CanAccessChannel(ctx, tenantID, userID, channelID)
	if err != nil {
		return false, err
	}
	return granted, nil
}

// AccessibleChannelIDs returns the ids of every channel instance userID
// may act on within tenantID, ordered by the Repository's deterministic
// listing. A gerente gets every channel; an atendente gets every open
// (non-restricted) channel plus the restricted channels they hold an
// explicit grant for. This is the filter primitive the P4 inbox read path
// applies to ListConversationSummaries.
//
// isGerente is threaded in for the same reason as CanAccessChannel.
func (s *AccessService) AccessibleChannelIDs(ctx context.Context, tenantID, userID uuid.UUID, isGerente bool) ([]uuid.UUID, error) {
	views, err := s.AccessibleChannels(ctx, tenantID, userID, isGerente)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(views))
	for _, v := range views {
		out = append(out, v.ID)
	}
	return out, nil
}

// ChannelView is the id + display-name projection of a channel instance
// the caller may act on. It backs the P4 inbox channel-scope filter chip:
// AccessibleChannels returns it so the web layer renders the chip options
// (name) and applies the read-path filter (id) from a single call,
// without a second query for names.
type ChannelView struct {
	ID          uuid.UUID
	DisplayName string
}

// AccessibleChannels returns every channel instance userID may act on
// within tenantID as an id + display-name projection, ordered by the
// Repository's deterministic listing. It applies the same rule as
// AccessibleChannelIDs (which delegates here): a gerente gets every
// channel; an atendente gets every open (non-restricted) channel plus the
// restricted channels they hold an explicit grant for. This is the
// primitive the P4 inbox read path + filter chip consume.
//
// isGerente is threaded in for the same reason as CanAccessChannel.
func (s *AccessService) AccessibleChannels(ctx context.Context, tenantID, userID uuid.UUID, isGerente bool) ([]ChannelView, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels: AccessibleChannels: tenant id is nil")
	}
	list, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelView, 0, len(list))
	if isGerente {
		for _, ch := range list {
			if ch != nil {
				out = append(out, ChannelView{ID: ch.ID, DisplayName: ch.DisplayName})
			}
		}
		return out, nil
	}
	grantedIDs, err := s.grants.ListAccessibleChannelIDs(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	granted := make(map[uuid.UUID]struct{}, len(grantedIDs))
	for _, id := range grantedIDs {
		granted[id] = struct{}{}
	}
	for _, ch := range list {
		if ch == nil {
			continue
		}
		if !ch.Restricted {
			out = append(out, ChannelView{ID: ch.ID, DisplayName: ch.DisplayName})
			continue
		}
		if _, ok := granted[ch.ID]; ok {
			out = append(out, ChannelView{ID: ch.ID, DisplayName: ch.DisplayName})
		}
	}
	return out, nil
}
