// Package slack is the Slack-webhook adapter that implements
// `alert.Alerter` against an Incoming Webhook URL targeting the
// `#security` channel ([SIN-62805] F2-05d).
//
// The adapter is intentionally minimal:
//
//   - POST a JSON body with the structured `text` plus a `blocks`
//     array. Slack treats `text` as the screen-reader/notification
//     fallback; `blocks` carries the human-readable formatted message.
//   - No retries inside the adapter. The worker treats a non-nil
//     Notify error as a redeliverable failure; the NATS broker
//     redelivers and the worker calls us again. Adding a retry loop
//     here would double-amplify Slack's own outage backoff.
//   - Secrets stay out of logs. The webhook URL is never logged; only
//     the response status is, on failure.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/alert"
)

// Config configures a Slack alerter. WebhookURL is required; HTTPClient
// is optional (tests inject httptest.Server.Client()).
type Config struct {
	WebhookURL string
	HTTPClient *http.Client
}

// MediaAlerter is the Slack adapter. Construct via New.
type MediaAlerter struct {
	url string
	hc  *http.Client
}

var _ alert.Alerter = (*MediaAlerter)(nil)

// NewMediaAlerter validates cfg and returns a MediaAlerter ready for
// use. Named distinctly from this package's other constructor (`New`
// for the circuit-breaker `Webhook`) so the media-scan media alerter
// and the issuance breaker can coexist in the same `slack` package
// without name collisions.
func NewMediaAlerter(cfg Config) (*MediaAlerter, error) {
	if cfg.WebhookURL == "" {
		return nil, errors.New("slack: Config.WebhookURL is required")
	}
	if !strings.HasPrefix(cfg.WebhookURL, "https://") && !strings.HasPrefix(cfg.WebhookURL, "http://") {
		return nil, errors.New("slack: Config.WebhookURL must be an absolute http(s) URL")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &MediaAlerter{url: cfg.WebhookURL, hc: hc}, nil
}

// payload is the JSON shape Slack's Incoming Webhook accepts. We use
// the basic `text` + `blocks` form (no advanced attachment legacy).
type payload struct {
	Text   string  `json:"text"`
	Blocks []block `json:"blocks"`
}

type block struct {
	Type string    `json:"type"`
	Text *textElem `json:"text,omitempty"`
}

type textElem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Notify POSTs a JSON body describing the infection event to the
// configured webhook. Empty events return alert.ErrEmptyEvent rather
// than wasting a Slack call.
func (a *MediaAlerter) Notify(ctx context.Context, e alert.Event) error {
	if e.TenantID == uuid.Nil || e.MessageID == uuid.Nil {
		return alert.ErrEmptyEvent
	}

	signature := e.Signature
	if signature == "" {
		signature = "unknown signature"
	}
	body := payload{
		Text: fmt.Sprintf("Infected media quarantined (tenant=%s, message=%s)", e.TenantID, e.MessageID),
		Blocks: []block{
			{Type: "section", Text: &textElem{Type: "mrkdwn", Text: "*Infected media quarantined*"}},
			{Type: "section", Text: &textElem{Type: "mrkdwn", Text: fmt.Sprintf(
				"• tenant_id: `%s`\n• message_id: `%s`\n• engine_id: `%s`\n• signature: `%s`",
				e.TenantID, e.MessageID, e.EngineID, signature,
			)}},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("slack: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slack: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
