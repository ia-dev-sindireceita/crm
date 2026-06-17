// Package middlewaretest provides test-only helpers that need access
// to internals of the middleware package. Importing this package is a
// build-time signal of test scope: every exported symbol takes a
// testing.TB parameter, which cannot be obtained outside a Go test
// run, so the helper is unreachable from non-test code.
//
// SIN-63978 / SIN-63956 §F3: replaces the prior
// middleware.WithActiveImpersonationForTest, whose forbidding doc
// comment was the only runtime barrier against accidental production
// use. Moving the helper into a sibling subpackage gated by
// testing.TB converts that convention into a compile-time guarantee.
package middlewaretest

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
)

// WithActiveImpersonation installs an active impersonation envelope
// on ctx, exactly as the production middleware would. The testing.TB
// parameter prevents non-test code from reaching this helper.
//
// Pass tb so the helper records itself as a test helper (tb.Helper()):
// failures inside production-only assertions then point at the
// real test call site, not at this wrapper.
func WithActiveImpersonation(tb testing.TB, ctx context.Context, s *impersonation.Session) context.Context {
	tb.Helper()
	return middleware.InstallActiveImpersonationForTest(ctx, s)
}
