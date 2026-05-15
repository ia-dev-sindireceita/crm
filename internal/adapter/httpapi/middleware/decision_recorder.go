package middleware

import (
	"context"

	"github.com/pericles-luz/crm/internal/iam"
)

// DecisionRecorder is a per-request, mutable sink for the iam.Decision
// produced by RequireAction. It exists because Go context values only
// flow inward: an outer middleware (the audit chain in [SIN-62254])
// that needs the Decision AFTER RequireAction returns cannot read
// context values that an inner handler attached, and RequireAction
// returns early via http.Error on deny — so the deny path never calls
// next.ServeHTTP with the updated request and DecisionFromContext is
// useless there.
//
// The recorder closes that gap: the outer middleware constructs a
// recorder, attaches it via WithDecisionRecorder, and reads
// .Decision() after next.ServeHTTP returns. RequireAction records on
// BOTH allow and deny paths before any response is written, so 100%
// of authorization outcomes become observable to the outer chain.
//
// The recorder is single-goroutine by design: every standard
// net/http middleware step for a given request runs on the same
// goroutine, so no mutex is needed. Consumers that hand the recorder
// off to a background goroutine MUST add their own synchronisation.
type DecisionRecorder struct {
	decision iam.Decision
	set      bool
}

// Record captures d. The most recent call wins, which matches how
// RequireAction is composed at most once per route: the final
// Decision is the one whose ReasonCode drove the response.
//
// Safe to call on a nil receiver, so callers can do
// `rec.Record(d)` without nil-checking when the recorder was looked
// up via DecisionRecorderFromContext on a request that may or may
// not have one.
func (r *DecisionRecorder) Record(d iam.Decision) {
	if r == nil {
		return
	}
	r.decision = d
	r.set = true
}

// Decision returns the recorded Decision and whether one was ever
// recorded. The bool MUST be checked — a zero Decision is not a
// valid "allow" signal.
//
// Safe to call on a nil receiver: returns the zero Decision and
// false.
func (r *DecisionRecorder) Decision() (iam.Decision, bool) {
	if r == nil {
		return iam.Decision{}, false
	}
	return r.decision, r.set
}

type decisionRecorderCtxKey struct{}

// WithDecisionRecorder attaches rec to ctx so RequireAction (or any
// other authorization site that wants to publish a Decision) can
// record into it without depending on a specific writer wrapper or a
// specific chain order.
func WithDecisionRecorder(ctx context.Context, rec *DecisionRecorder) context.Context {
	return context.WithValue(ctx, decisionRecorderCtxKey{}, rec)
}

// DecisionRecorderFromContext returns the recorder attached by
// WithDecisionRecorder. The bool is false when no recorder is in
// context OR when a nil pointer was attached — callers can treat
// "ok" as "I have a non-nil recorder I can record into".
func DecisionRecorderFromContext(ctx context.Context) (*DecisionRecorder, bool) {
	rec, ok := ctx.Value(decisionRecorderCtxKey{}).(*DecisionRecorder)
	if !ok || rec == nil {
		return nil, false
	}
	return rec, true
}
