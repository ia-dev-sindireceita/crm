package funnel

import "errors"

// ErrInvalidTenant is returned when tenantID is uuid.Nil. Every funnel
// row is tenant-scoped; an anonymous transition is a programming bug.
var ErrInvalidTenant = errors.New("funnel: invalid tenant id")

// ErrInvalidConversation is returned when conversationID is uuid.Nil.
// The funnel addresses conversations across the inbox boundary by id;
// a zero id cannot resolve to anything sensible.
var ErrInvalidConversation = errors.New("funnel: invalid conversation id")

// ErrInvalidActor is returned when the user id that moved the
// conversation is uuid.Nil. funnel_transition.transitioned_by_user_id
// is NOT NULL — the durable who-did-what trail demands an actor.
var ErrInvalidActor = errors.New("funnel: invalid actor user id")

// ErrInvalidStageKey is returned when the destination stage key is
// blank after trimming. Keys are stable identifiers like "novo" or
// "ganho"; addressing a stage by an empty string is always a bug.
var ErrInvalidStageKey = errors.New("funnel: invalid stage key")

// ErrStageNotFound is returned when MoveConversation cannot resolve
// the destination stage key under the tenant scope. Callers can
// errors.Is against this sentinel to render a 404 / "stage unknown"
// without parsing strings.
var ErrStageNotFound = errors.New("funnel: stage not found")

// ErrNotFound is the storage-layer sentinel for "no row matched".
// Repositories return it for missing lookups; the service translates
// it into the more specific ErrStageNotFound when appropriate.
var ErrNotFound = errors.New("funnel: not found")
