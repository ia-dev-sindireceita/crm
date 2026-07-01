package main

// SIN-66378 P4 — inbound channel routing wireup.
//
// wireChannelResolver attaches the tenant-channel-instance router to a
// ReceiveInbound so a newly-created conversation references the
// tenant_channels row resolved from the inbound identity (channel_id)
// rather than the bare carrier string. That is what lets two numbers of
// the same carrier live side by side without colliding, and it is the
// column the per-channel access filter (the live inbox read path) scopes.
//
// It is soft-fail by design — matching the other optional inbound hooks
// (campaign linker, funnel publisher): a build error leaves routing
// disabled (channel_id stays NULL, the pre-P4 behaviour) and logs a warn,
// never aborting the carrier wireup. The resolver itself is also
// soft-fail per delivery: an unresolved identity leaves the conversation
// unrouted rather than dropping the message (see
// inboxusecase.ReceiveInbound.routeConversation).

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	pgchannels "github.com/pericles-luz/crm/internal/adapter/db/postgres/channels"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// wireChannelResolver builds the channels Postgres adapter from pool and
// registers it as the ReceiveInbound routing port. Both arguments are
// expected non-nil in production; a nil pool or an adapter build error is
// logged and skipped so the inbound path keeps working unrouted.
func wireChannelResolver(receiver *inboxusecase.ReceiveInbound, pool *pgxpool.Pool) {
	if receiver == nil || pool == nil {
		return
	}
	store, err := pgchannels.New(pool)
	if err != nil {
		slog.Default().Warn("inbox wire: channel resolver disabled", "err", err)
		return
	}
	receiver.SetChannelResolver(store)
	receiver.SetChannelResolverLogger(slog.Default())
}
