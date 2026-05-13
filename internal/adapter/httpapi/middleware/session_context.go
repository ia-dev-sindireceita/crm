package middleware

import (
	"context"

	"github.com/pericles-luz/crm/internal/iam"
)

// sessionCtxKey is the unexported context-key type for the resolved
// session. Kept in this package so handlers in sibling packages must call
// SessionFromContext rather than reach for the raw key.
//
// The full Auth middleware (cookie validation → session store lookup) is
// re-landed in batch 10 alongside the sessioncookie adapter it depends on.
// This file holds only the context helpers so the Impersonation middleware
// can compile independently.
type sessionCtxKey struct{}

// WithSession attaches the validated session to ctx for downstream
// handlers. Tests use it to seed contexts without going through the full
// Auth chain.
func WithSession(ctx context.Context, s iam.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// SessionFromContext returns the session injected by the Auth middleware.
// The bool is false when the request never went through Auth — callers
// SHOULD treat that as a programmer error rather than a 401 path.
func SessionFromContext(ctx context.Context) (iam.Session, bool) {
	s, ok := ctx.Value(sessionCtxKey{}).(iam.Session)
	return s, ok
}
