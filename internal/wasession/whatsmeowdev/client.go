package whatsmeowdev

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// waClient is the subset of *whatsmeow.Client the device uses at runtime.
// *whatsmeow.Client satisfies it directly, so the production path needs no
// wrapper; tests supply an in-memory fake. AddEventHandler is intentionally
// not here — it is wired once at construction by the factory, which holds the
// concrete client.
type waClient interface {
	IsLoggedIn() bool
	Connect() error
	Disconnect()
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// compile-time assertion that the real client implements the seam.
var _ waClient = (*whatsmeow.Client)(nil)
