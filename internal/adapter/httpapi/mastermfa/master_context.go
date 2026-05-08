package mastermfa

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Master is the minimum information the enrol / verify / recovery
// handlers need about the currently-authenticated master operator.
// More fields (e.g. role, last_login) can be added without breaking
// callers as long as the field set stays additive.
type Master struct {
	ID    uuid.UUID
	Email string
}

// ErrNoMaster is the sentinel returned when no Master is bound to
// the request context. The handlers map this to 401 / login redirect
// (deny-by-default per ADR 0074 §3) rather than 500 — a missing
// master is an authn failure, not a bug.
var ErrNoMaster = errors.New("mastermfa: no master in request context")

// masterCtxKey is the unexported key for the per-request master
// value. Callers in sibling packages MUST go through MasterFromContext
// rather than reach for the raw key.
type masterCtxKey struct{}

// WithMaster attaches a Master to ctx. The future master-auth
// middleware (a sibling of httpapi/middleware/auth.go but for the
// master subtree) wires this; tests use it directly to skip the
// middleware in unit-level tests.
func WithMaster(ctx context.Context, m Master) context.Context {
	return context.WithValue(ctx, masterCtxKey{}, m)
}

// MasterFromContext returns the master injected by an upstream
// middleware. The bool is false when the request never went through
// master auth — handlers MUST treat that as a 401 redirect, not a
// 500.
func MasterFromContext(ctx context.Context) (Master, bool) {
	m, ok := ctx.Value(masterCtxKey{}).(Master)
	if !ok || m.ID == uuid.Nil {
		return Master{}, false
	}
	return m, true
}
