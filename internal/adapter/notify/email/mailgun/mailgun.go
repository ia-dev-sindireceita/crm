// Package mailgun implements email.EmailSender on top of the Mailgun
// (Sinch) HTTP REST API (https://documentation.mailgun.com/). The
// adapter is the production wiring picked by the boot factory when
// EMAIL_PROVIDER=mailgun (decision #9 of plan rev 3, ratified by the
// board in [SIN-62206]).
//
// Choice: net/http against the JSON-friendly multipart REST API
// directly, instead of github.com/mailgun/mailgun-go/v4. Justification
// is in docs/adr/0091-email-mailgun.md — the relevant points are
// matching the existing slack-adapter precedent, keeping the supply
// chain narrow (govulncheck surface), and the messages endpoint being
// a thin form-post that doesn't earn a 50-file SDK.
//
// Security:
//   - API key is read at construction (env), never logged, never
//     placed in URLs. HTTP basic auth with username "api".
//   - All calls are HTTPS. TLS verification is the http.Transport
//     default (no InsecureSkipVerify).
//   - The adapter logs only structural metadata: recipient count,
//     payload size, response status, mailgun message-id. Subject and
//     body are NEVER logged.
//   - Validation runs first; an invalid Message never touches the
//     network.
package mailgun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/notify/email"
)

// Region selects the Mailgun API endpoint. Mailgun runs distinct US
// and EU regions with separate domains; sending to the wrong region
// returns 404. The two values match the public docs.
type Region string

const (
	RegionUS Region = "us"
	RegionEU Region = "eu"
)

// baseURL returns the Mailgun API root for r. Returns "" for an
// unknown region; New rejects that explicitly so callers see a
// configuration error at boot, not a runtime 404.
func (r Region) baseURL() string {
	switch r {
	case RegionUS:
		return "https://api.mailgun.net"
	case RegionEU:
		return "https://api.eu.mailgun.net"
	default:
		return ""
	}
}

// DefaultTimeout caps the round-trip duration for a single Send call.
// Mailgun's message endpoint typically responds in <1s; this is wide
// enough to absorb spikes without stalling caller-side request paths.
const DefaultTimeout = 15 * time.Second

// Doer is the narrow subset of http.Client the adapter needs. Tests
// substitute a fake httptest.Server through New + a custom client
// configured by WithClient.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config holds the values resolved by the boot factory. Domain is the
// Mailgun sending domain (e.g. "mg.acme.com") and APIKey is the
// region-private API key (HTTP basic auth username is the literal
// string "api"). BaseURL overrides the region endpoint and exists
// solely so tests can point Sender at an httptest.Server; production
// code MUST leave it empty.
type Config struct {
	APIKey  string
	Domain  string
	Region  Region
	BaseURL string
}

// Sender is the Mailgun email.EmailSender. Construct with New.
type Sender struct {
	apiKey  string
	domain  string
	baseURL string
	client  Doer
	timeout time.Duration
	logger  *slog.Logger
}

// Compile-time port assertion.
var _ email.EmailSender = (*Sender)(nil)

// ErrMissingConfig is returned by New when a required Config field is
// empty. Boot wiring re-wraps it with the offending env var name so
// the operator sees "MAILGUN_API_KEY missing" rather than a generic
// "invalid config".
var ErrMissingConfig = errors.New("mailgun: missing configuration")

// New validates cfg and returns a Sender. Empty APIKey, empty Domain,
// or unknown Region all return ErrMissingConfig. The defaults pick a
// 15s timeout and slog.Default(); use WithClient/WithTimeout/WithLogger
// to override for tests.
func New(cfg Config) (*Sender, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%w: APIKey is required", ErrMissingConfig)
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("%w: Domain is required", ErrMissingConfig)
	}
	base := cfg.BaseURL
	if base == "" {
		base = cfg.Region.baseURL()
		if base == "" {
			return nil, fmt.Errorf("%w: Region must be %q or %q (got %q)", ErrMissingConfig, RegionUS, RegionEU, cfg.Region)
		}
	}
	return &Sender{
		apiKey:  cfg.APIKey,
		domain:  cfg.Domain,
		baseURL: strings.TrimRight(base, "/"),
		client:  &http.Client{Timeout: DefaultTimeout},
		timeout: DefaultTimeout,
		logger:  slog.Default(),
	}, nil
}

// WithClient returns a copy of s wired with doer for HTTP. Tests use
// this to point at httptest.Server without touching the constructor.
func (s *Sender) WithClient(doer Doer) *Sender {
	cp := *s
	cp.client = doer
	return &cp
}

// WithTimeout returns a copy of s with a different per-call deadline.
// Tests shorten it to keep the suite fast; ops can extend it for
// larger attachment payloads. A non-positive t resets the per-call
// deadline to DefaultTimeout so a stray WithTimeout(0) never produces
// an immediately-cancelled context.
func (s *Sender) WithTimeout(t time.Duration) *Sender {
	cp := *s
	if t <= 0 {
		t = DefaultTimeout
	}
	cp.timeout = t
	return &cp
}

// WithLogger returns a copy of s that emits structured logs to l.
// Production wiring passes the obs/log redacting logger; tests pass a
// discarding logger to keep output clean.
func (s *Sender) WithLogger(l *slog.Logger) *Sender {
	cp := *s
	if l != nil {
		cp.logger = l
	}
	return &cp
}

// mailgunResponse is the JSON shape returned by /messages. id and
// message are documented; we ignore other keys deliberately.
type mailgunResponse struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Send delivers msg through the Mailgun /v3/{domain}/messages API.
//
// Error contract maps onto the port sentinels:
//   - msg.Validate failure → wrapped email.ErrInvalidMessage
//   - HTTP 2xx → nil
//   - HTTP 4xx (excluding 408, 429) → wrapped email.ErrPermanent
//   - HTTP 408, 429, 5xx → wrapped email.ErrTransient
//   - network / context errors → wrapped email.ErrTransient
func (s *Sender) Send(ctx context.Context, msg email.Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	body, contentType, err := buildMultipart(msg)
	if err != nil {
		return fmt.Errorf("mailgun: build payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	url := s.baseURL + "/v3/" + s.domain + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mailgun: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth("api", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		s.logSendFailure(msg, 0, err)
		return fmt.Errorf("%w: %v", email.ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		var decoded mailgunResponse
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		s.logSendOK(msg, len(body), resp.StatusCode, decoded.ID)
		return nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bucket := classify(resp.StatusCode)
	s.logSendFailure(msg, resp.StatusCode, errors.New(string(respBody)))
	return fmt.Errorf("%w: mailgun status %d: %s", bucket, resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// classify maps a non-2xx HTTP status onto the port's transient /
// permanent buckets. 408 (Request Timeout), 429 (Too Many Requests),
// and any 5xx are retryable per RFC 9110 and Mailgun docs; everything
// else (auth, malformed domain, banned recipient, or an unexpected
// 1xx/3xx) is treated as permanent so the caller does not loop on a
// misconfiguration.
func classify(status int) error {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return email.ErrTransient
	}
	return email.ErrPermanent
}

// buildMultipart serialises msg as a multipart/form-data body matching
// Mailgun's /messages REST contract:
//
//	from, to[], cc[], bcc[], subject, text, html, h:<header>,
//	attachment (file part).
//
// The boundary is chosen by mime/multipart so we never need to assert
// on it from the caller side.
//
// Implementation note: WriteField / CreatePart / Close return an error
// only when the underlying writer returns an error. We write to a
// bytes.Buffer (which never fails), so those returns are discarded.
// The only real failure path is io.Copy on a caller-supplied
// attachment reader; that error is surfaced verbatim.
func buildMultipart(msg email.Message) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	add := func(name, value string) { _ = w.WriteField(name, value) }

	add("from", msg.From.String())
	for _, addr := range msg.To {
		add("to", addr.String())
	}
	for _, addr := range msg.Cc {
		add("cc", addr.String())
	}
	for _, addr := range msg.Bcc {
		add("bcc", addr.String())
	}
	if msg.ReplyTo != nil {
		add("h:Reply-To", msg.ReplyTo.String())
	}
	add("subject", msg.Subject)
	if msg.Text != "" {
		add("text", msg.Text)
	}
	if msg.HTML != "" {
		add("html", msg.HTML)
	}
	for k, v := range msg.Headers {
		add("h:"+k, v)
	}
	for _, att := range msg.Attachments {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment"; filename=%q`, att.Filename))
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		hdr.Set("Content-Type", ct)
		part, _ := w.CreatePart(hdr)
		if att.Content != nil {
			if _, err := io.Copy(part, att.Content); err != nil {
				return nil, "", err
			}
		}
	}
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType(), nil
}

// logSendOK emits a single structured log line for a successful send.
// Subject, body, and recipient addresses are intentionally omitted —
// only counts and provider id leave the process.
func (s *Sender) logSendOK(msg email.Message, bytes, status int, providerID string) {
	s.logger.Info("mailgun: send ok",
		slog.Int("recipients", len(msg.To)+len(msg.Cc)+len(msg.Bcc)),
		slog.Int("attachments", len(msg.Attachments)),
		slog.Int("payload_bytes", bytes),
		slog.Int("status", status),
		slog.String("provider_id", providerID),
	)
}

// logSendFailure emits a structured failure line. The cause is reduced
// to its Error() string and is expected to come from Mailgun (4xx/5xx
// JSON body) or the network layer — never from caller-supplied data.
func (s *Sender) logSendFailure(msg email.Message, status int, cause error) {
	attrs := []slog.Attr{
		slog.Int("recipients", len(msg.To)+len(msg.Cc)+len(msg.Bcc)),
		slog.Int("attachments", len(msg.Attachments)),
		slog.Int("status", status),
		slog.String("cause", cause.Error()),
	}
	s.logger.LogAttrs(context.Background(), slog.LevelError, "mailgun: send failed", attrs...)
}
