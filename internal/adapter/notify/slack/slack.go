// Package slack implements iam/ratelimit.Alerter as a synchronous
// Slack incoming-webhook POST. The only producer today is the master
// account-lockout path (SIN-62341 acceptance criterion #3): a 5xx-ish
// "5 master logins failed → locked for 30m" event is delivered to
// Slack in the same request goroutine that wrote the account_lockout
// row, so an operator sees the alert in real time without an
// asynchronous fan-out hop.
//
// This adapter is deliberately small: Slack is the only transport, the
// payload is a single text field, and the policy decision (which
// events to alert on, how often) lives in the caller. A future
// PagerDuty / Opsgenie adapter would be a sibling package, not a
// branch in here.
//
// The HTTP client is bounded by a 5s deadline so a misconfigured
// webhook does not stall the login response. Webhook URL is read at
// construction; an empty URL turns Notify into a no-op so callers can
// wire the adapter unconditionally and let the operator opt in via
// config.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// DefaultTimeout caps the round-trip duration for a single Notify call.
// Compared to the typical Slack webhook P99 (<1s) this is generous;
// compared to a misconfigured webhook (DNS hole, dead host) this is
// tight enough that the login response does not stall.
const DefaultTimeout = 5 * time.Second

// Doer is the narrow subset of http.Client the adapter needs. Tests
// substitute a fake without spinning up a real server (e.g. for
// non-2xx coverage); production wires *http.Client.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Notifier is the Slack-webhook Alerter adapter.
type Notifier struct {
	webhookURL string
	client     Doer
	timeout    time.Duration
}

// Compile-time assertion that *Notifier satisfies the domain port.
var _ ratelimit.Alerter = (*Notifier)(nil)

// New constructs a Notifier. An empty webhookURL is permitted — Notify
// becomes a no-op in that case so the call site can wire the adapter
// unconditionally and let the operator opt in via config (the same
// pattern as Postgres bootstrap when feature-flag-gated tables are
// optional).
//
// The default HTTP client carries a DefaultTimeout deadline. Use
// WithClient or WithTimeout to override for tests / specialised
// deployments.
func New(webhookURL string) *Notifier {
	return &Notifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: DefaultTimeout},
		timeout:    DefaultTimeout,
	}
}

// WithClient returns a copy of n that uses doer for HTTP. Tests inject
// a fake to assert non-2xx handling without a real server.
func (n *Notifier) WithClient(doer Doer) *Notifier {
	cp := *n
	cp.client = doer
	return &cp
}

// WithTimeout returns a copy of n whose per-call deadline is t. Tests
// shorten the timeout to keep the suite fast.
func (n *Notifier) WithTimeout(t time.Duration) *Notifier {
	cp := *n
	cp.timeout = t
	return &cp
}

// payload is the canonical Slack incoming-webhook body. The "text"
// field is the only required key; richer block-kit payloads are out
// of scope for this adapter.
type payload struct {
	Text string `json:"text"`
}

// Notify delivers msg to the configured Slack webhook. The supplied
// context is honoured; it is also wrapped with the adapter's timeout
// so a context-without-deadline cannot bypass the per-call limit.
//
// An empty webhook URL turns Notify into a no-op: the login flow can
// wire the adapter unconditionally and let the operator opt in via
// config, without a nil-check at the call site.
//
// Non-2xx responses surface as an error so the caller can log the
// delivery failure; a missing webhook is NOT an error.
func (n *Notifier) Notify(ctx context.Context, msg string) error {
	if n.webhookURL == "" {
		return nil
	}
	body, err := json.Marshal(payload{Text: msg})
	if err != nil {
		return fmt.Errorf("slack: marshal payload: %w", err)
	}

	timeout := n.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: post webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack: webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// ErrInvalidConfig is returned by callers that want to escalate a
// configuration mistake (e.g. an empty webhook URL when the operator
// expected an alert). This adapter does not produce it directly —
// New tolerates an empty URL on purpose — but it is exported so a
// boot-time gate can use it consistently.
var ErrInvalidConfig = errors.New("slack: invalid configuration")
