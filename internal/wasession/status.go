package wasession

// Status is the lifecycle state of a per-tenant WhatsApp Web session.
//
// The required external signal (Fase 1 AC) is connected / disconnected /
// banned; Unpaired and Pairing are the two intermediate states the QR
// pairing flow moves through before a session can ever be Connected.
type Status string

const (
	// StatusUnpaired means no credentials are persisted for the tenant;
	// the session must be paired via QR before it can connect.
	StatusUnpaired Status = "unpaired"
	// StatusPairing means a QR code has been issued and the session is
	// waiting for the operator to scan it from the WhatsApp mobile app.
	StatusPairing Status = "pairing"
	// StatusConnected means the session is live and exchanging messages.
	StatusConnected Status = "connected"
	// StatusDisconnected means the session dropped its live connection but
	// still holds valid credentials; the supervisor will reconnect.
	StatusDisconnected Status = "disconnected"
	// StatusBanned means WhatsApp logged the session out (ban or remote
	// logout). It is terminal: the supervisor does not auto-reconnect a
	// banned session, the operator must re-pair.
	StatusBanned Status = "banned"
)

// Valid reports whether s is one of the known status values.
func (s Status) Valid() bool {
	switch s {
	case StatusUnpaired, StatusPairing, StatusConnected, StatusDisconnected, StatusBanned:
		return true
	default:
		return false
	}
}

// Terminal reports whether the status is one the supervisor must not try
// to recover from automatically. Only a banned session is terminal — an
// operator has to re-pair it.
func (s Status) Terminal() bool {
	return s == StatusBanned
}

// Live reports whether the session is currently exchanging messages.
func (s Status) Live() bool {
	return s == StatusConnected
}

// String implements fmt.Stringer.
func (s Status) String() string { return string(s) }
