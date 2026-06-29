package wasession

import "errors"

var (
	// ErrSessionExists is returned by StartSession when the tenant already
	// has a running session.
	ErrSessionExists = errors.New("wasession: session already running for tenant")
	// ErrSessionNotFound is returned by Send / StopSession / Status when no
	// session is running for the tenant.
	ErrSessionNotFound = errors.New("wasession: no session for tenant")
	// ErrManagerClosed is returned once the Manager has been shut down.
	ErrManagerClosed = errors.New("wasession: manager closed")
	// ErrNotConnected is returned by Send when the session exists but is
	// not currently connected.
	ErrNotConnected = errors.New("wasession: session not connected")
)
