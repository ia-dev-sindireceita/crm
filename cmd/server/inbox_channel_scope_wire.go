package main

// SIN-66378 P4 — inbox channel-scope wireup.
//
// buildInboxChannelScope adapts the channels access domain to the
// web/inbox ChannelScopeUseCase port so the live inbox read path
// (ListConversationSummaries) is filtered by the caller's accessible
// channel instances and the channel-scope filter chip renders the
// caller's channels. A gerente sees every channel; an atendente sees the
// open channels plus the restricted ones they hold an explicit grant for
// (channels.AccessService, ADR-0109).
//
// isGerenteFromSessionContext feeds the role bit the access rule needs,
// read from the session installed by middleware.Auth.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgchannels "github.com/pericles-luz/crm/internal/adapter/db/postgres/channels"
	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/iam"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// inboxChannelScope adapts channels.AccessService to the web/inbox
// ChannelScopeUseCase port, projecting the domain ChannelView onto the
// web-local AccessibleChannel so the handler package need not import the
// channels domain.
type inboxChannelScope struct {
	access *channels.AccessService
}

// AccessibleChannels implements webinbox.ChannelScopeUseCase.
func (s inboxChannelScope) AccessibleChannels(ctx context.Context, tenantID, userID uuid.UUID, isGerente bool) ([]webinbox.AccessibleChannel, error) {
	views, err := s.access.AccessibleChannels(ctx, tenantID, userID, isGerente)
	if err != nil {
		return nil, err
	}
	out := make([]webinbox.AccessibleChannel, 0, len(views))
	for _, v := range views {
		out = append(out, webinbox.AccessibleChannel{ID: v.ID, DisplayName: v.DisplayName})
	}
	return out, nil
}

// buildInboxChannelScope constructs the ChannelScope port from the
// runtime pool. Returns nil (soft-fail) when the channels adapter or the
// access service cannot be built so the inbox surface degrades to the
// pre-P4 tenant-wide list rather than failing the boot — matching the
// other optional inbox collaborators.
func buildInboxChannelScope(pool *pgxpool.Pool) webinbox.ChannelScopeUseCase {
	store, err := pgchannels.New(pool)
	if err != nil {
		slog.Default().Warn("inbox wire: channel-scope filter disabled — channels store", "err", err)
		return nil
	}
	access, err := channels.NewAccessService(store, store)
	if err != nil {
		slog.Default().Warn("inbox wire: channel-scope filter disabled — access service", "err", err)
		return nil
	}
	return inboxChannelScope{access: access}
}

// isGerenteFromSessionContext reports whether the request principal holds
// the tenant gerente role, the bit the P4 per-channel access filter needs
// to grant the see-all override. A missing session fails safe to false
// (atendente scope / deny-by-default), never see-all.
func isGerenteFromSessionContext(r *http.Request) bool {
	return roleFromSessionContext(r) == iam.RoleTenantGerente
}

// Compile-time guard: the adapter satisfies the web/inbox port.
var _ webinbox.ChannelScopeUseCase = inboxChannelScope{}
