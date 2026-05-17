package engine

import "errors"

// ErrInvalidEvent flags a wire payload the engine cannot decode or
// whose required fields are empty / Nil. Callers (the worker handler)
// treat this as a poison delivery: log + Ack so JetStream stops
// redelivering a body that will never decode.
var ErrInvalidEvent = errors.New("engine: invalid inbound message event")

// ErrAlreadyApplied signals that the (rule_id, message_id) pair is
// already on the applications ledger. The [ApplicationsRepo.Record]
// adapter raises this on a UNIQUE conflict; [Engine.Handle] treats it
// as a successful no-op (the previous delivery already applied the
// action).
var ErrAlreadyApplied = errors.New("engine: rule application already recorded")

// ErrInvalidConfig is returned by [NewEngine] when a required
// dependency is missing. The constructor pattern mirrors
// funnel.NewService so cmd/server fails fast at boot on
// mis-configuration instead of nil-panicking on first handle.
var ErrInvalidConfig = errors.New("engine: invalid config")
