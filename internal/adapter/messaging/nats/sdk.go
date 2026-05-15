// SDK glue between `github.com/nats-io/nats.go` and the worker's
// narrow Subscriber/Publisher contracts (internal/media/worker). The
// scope is intentionally limited to what the mediascan-worker needs:
//
//   - Dial a NATS server + open a JetStream context.
//   - EnsureStream a JetStream stream with a 1h Duplicates window so
//     the F-14 invariant the existing Publisher relies on continues
//     to hold for the media-scan subjects too.
//   - Publish raw JSON bodies on a subject (no dedup header; the
//     worker re-publishes on redelivery and downstream dedups).
//   - QueueSubscribe with manual ack and a per-message handler that
//     receives a *Delivery wrapping the underlying *nats.Msg.
//   - Close drains the conn so in-flight messages get a chance to
//     finish before the process exits (graceful shutdown contract).
//
// The package keeps the older webhook.Publisher (JetStream interface +
// dedup) intact — that adapter has its own deduplication and stream
// management — and adds SDKAdapter as a sibling for at-least-once
// pipelines that just need straightforward queue groups.

package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// SDKConfig configures Connect. URL is required; everything else has
// sensible defaults.
//
// The transport-security posture is secure-by-default ([SIN-62815]):
// a zero-value SDKConfig will not connect. Callers MUST either
//
//   - point URL at a tls:// (or wss://) endpoint AND set TLSCAFile, OR
//   - explicitly set Insecure=true to acknowledge a plaintext deployment.
//
// At most one auth method (Token, NKeyFile, CredsFile) may be set. The
// preferred option for production is CredsFile — `.creds` JWT files can
// be rotated on disk without touching the binary, and the deployed
// credential can be scoped to a single subject (least privilege).
type SDKConfig struct {
	// URL is the NATS endpoint (e.g. "tls://nats.example:4222").
	// Plaintext schemes (nats://, ws://) require Insecure=true.
	URL string
	// Name is the human-friendly client name surfaced by NATS server
	// monitoring. Defaults to "crm-mediascan-worker".
	Name string
	// ConnectTimeout caps the initial dial. Defaults to 10s.
	ConnectTimeout time.Duration
	// MaxReconnects caps automatic reconnects. -1 means forever.
	MaxReconnects int
	// ReconnectWait is the per-attempt delay. Defaults to 2s.
	ReconnectWait time.Duration

	// --- Auth (mutually exclusive; pick one) ---

	// Token is a shared-secret bearer token. Operational footgun:
	// rotating the token requires restarting the process. Prefer
	// CredsFile in production.
	Token string
	// NKeyFile is a filesystem path to an NKey seed. The seed never
	// leaves the file; the SDK signs nonces in-memory.
	NKeyFile string
	// CredsFile is a filesystem path to a chained .creds (JWT + NKey)
	// file. Preferred for production: supports decentralized auth and
	// can be rotated by replacing the file on disk between reconnects.
	CredsFile string

	// --- TLS surface ---

	// TLSCAFile is the path to a PEM bundle of CAs the client trusts.
	// Required when URL scheme is tls:// or wss:// (unless Insecure).
	TLSCAFile string
	// TLSCertFile is the client certificate for mTLS (optional). Must
	// be set together with TLSKeyFile.
	TLSCertFile string
	// TLSKeyFile is the private key for the client certificate.
	TLSKeyFile string

	// Insecure is the explicit opt-out from the secure-by-default
	// posture. When true, plaintext transport and missing auth are
	// permitted. Required for dev/in-cluster deployments that ride a
	// private network without TLS termination at the broker. Production
	// deploys MUST NOT set this — pre-deploy review will block it.
	Insecure bool
}

// validate enforces the secure-by-default posture and the auth/TLS
// mutual-exclusion rules. It runs before any socket is opened so a
// misconfigured deploy fails fast at process start instead of after a
// reconnect storm.
func (c SDKConfig) validate() error {
	if c.URL == "" {
		return errors.New("nats: SDKConfig.URL is required")
	}

	// Auth mutual exclusion. Empty auth is permitted only when the
	// caller has explicitly opted into Insecure transport.
	authMethods := 0
	if c.Token != "" {
		authMethods++
	}
	if c.NKeyFile != "" {
		authMethods++
	}
	if c.CredsFile != "" {
		authMethods++
	}
	if authMethods > 1 {
		return errors.New("nats: SDKConfig has multiple auth methods set; choose one of Token, NKeyFile, CredsFile")
	}

	// mTLS cert/key must be set together (a key without a cert or
	// vice-versa is always a configuration mistake).
	if (c.TLSCertFile != "") != (c.TLSKeyFile != "") {
		return errors.New("nats: SDKConfig.TLSCertFile and TLSKeyFile must be set together")
	}

	scheme := schemeOf(c.URL)
	hasTLSConfig := c.TLSCAFile != ""

	switch scheme {
	case "tls", "wss":
		// Secure scheme without a CA bundle means we'd be trusting the
		// system roots — fine in some envs but a footgun on bespoke
		// VPS deploys. Require an explicit bundle unless Insecure
		// acknowledges the trade-off.
		if !hasTLSConfig && !c.Insecure {
			return fmt.Errorf("nats: %s:// URL requires TLSCAFile (or set Insecure=true to bypass)", scheme)
		}
	case "nats", "ws", "":
		// Plaintext or scheme-less URL. Refuse unless the caller has
		// explicitly accepted the insecure posture.
		if !c.Insecure {
			return errors.New("nats: refusing plaintext URL; set Insecure=true or use a tls:// URL with TLSCAFile")
		}
	default:
		return fmt.Errorf("nats: URL scheme %q is not supported", scheme)
	}

	// When secure transport is on, require an auth method too. The
	// goal is to make "no auth + secure" impossible to fall into by
	// accident; if a deploy genuinely needs anonymous TLS it can pass
	// Insecure=true and own the deviation.
	if !c.Insecure && authMethods == 0 {
		return errors.New("nats: refusing connection without auth; set Token, NKeyFile, or CredsFile (or Insecure=true to bypass)")
	}

	return nil
}

// schemeOf returns the URL scheme without parsing — nats.go accepts a
// comma-separated server list, and we only care about the first entry's
// scheme for the security check.
func schemeOf(url string) string {
	if i := strings.Index(url, ","); i >= 0 {
		url = url[:i]
	}
	if i := strings.Index(url, "://"); i >= 0 {
		return strings.ToLower(url[:i])
	}
	return ""
}

// SDKAdapter wraps a live *nats.Conn + JetStream context. One per
// process. Safe for concurrent Publish and Subscribe calls.
type SDKAdapter struct {
	conn *natsgo.Conn
	js   natsgo.JetStreamContext
}

// Connect dials NATS, opens a JetStream context, and returns an
// SDKAdapter ready for use. The caller MUST Close the adapter on
// shutdown.
//
// Connect validates the transport-security posture (see
// SDKConfig.validate) before opening any socket: a misconfigured deploy
// fails fast at startup, not after a reconnect storm.
func Connect(_ context.Context, cfg SDKConfig) (*SDKAdapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Name == "" {
		cfg.Name = "crm-mediascan-worker"
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.ReconnectWait <= 0 {
		cfg.ReconnectWait = 2 * time.Second
	}

	opts := []natsgo.Option{
		natsgo.Name(cfg.Name),
		natsgo.Timeout(cfg.ConnectTimeout),
		natsgo.ReconnectWait(cfg.ReconnectWait),
		natsgo.MaxReconnects(cfg.MaxReconnects),
	}

	// Auth options. validate() guarantees at most one is set.
	switch {
	case cfg.CredsFile != "":
		opts = append(opts, natsgo.UserCredentials(cfg.CredsFile))
	case cfg.NKeyFile != "":
		nkeyOpt, err := natsgo.NkeyOptionFromSeed(cfg.NKeyFile)
		if err != nil {
			// Wrap without echoing file contents — the path is fine
			// to log but the seed bytes inside MUST NOT be surfaced
			// even on error.
			return nil, fmt.Errorf("nats: nkey seed from %q: %w", cfg.NKeyFile, err)
		}
		opts = append(opts, nkeyOpt)
	case cfg.Token != "":
		opts = append(opts, natsgo.Token(cfg.Token))
	}

	// TLS options. nats.go's nats:// scheme silently ignores TLS
	// options, so it's safe (if odd) to pass these alongside an
	// Insecure plaintext URL; validate() already blocks that for the
	// secure schemes.
	if cfg.TLSCAFile != "" {
		opts = append(opts, natsgo.RootCAs(cfg.TLSCAFile))
	}
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		opts = append(opts, natsgo.ClientCert(cfg.TLSCertFile, cfg.TLSKeyFile))
	}

	conn, err := natsgo.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %q: %w", cfg.URL, err)
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("nats: jetstream context: %w", err)
	}
	return &SDKAdapter{conn: conn, js: js}, nil
}

// EnsureStream creates the stream if it does not exist, otherwise
// no-ops. The Duplicates window is fixed at 1h to align with the
// reconciler tolerance set by the webhook Publisher (F-14).
//
// Idempotent: a second call against an existing stream returns nil
// without attempting a reconfigure (the worker should not silently
// flip operator-tuned retention / replicas).
func (a *SDKAdapter) EnsureStream(name string, subjects []string) error {
	if name == "" {
		return errors.New("nats: stream name is required")
	}
	if len(subjects) == 0 {
		return errors.New("nats: stream subjects required")
	}
	if _, err := a.js.StreamInfo(name); err == nil {
		return nil
	}
	_, err := a.js.AddStream(&natsgo.StreamConfig{
		Name:       name,
		Subjects:   subjects,
		Storage:    natsgo.FileStorage,
		Retention:  natsgo.WorkQueuePolicy,
		Duplicates: MinDuplicatesWindow,
	})
	if err != nil {
		return fmt.Errorf("nats: add stream %q: %w", name, err)
	}
	return nil
}

// Publish sends body on subject with at-least-once delivery into the
// owning JetStream stream. No dedup header is set; redelivery of
// `media.scan.completed` from the worker is expected, and downstream
// consumers (SIN-62805) dedup on (tenant_id, message_id).
func (a *SDKAdapter) Publish(ctx context.Context, subject string, body []byte) error {
	if subject == "" {
		return errors.New("nats: subject is required")
	}
	if _, err := a.js.PublishMsg(&natsgo.Msg{Subject: subject, Data: body}, natsgo.Context(ctx)); err != nil {
		return fmt.Errorf("nats: publish %q: %w", subject, err)
	}
	return nil
}

// HandlerFunc is the per-message callback shape Subscribe wires. The
// adapter does NOT ack on its behalf; the worker calls Delivery.Ack
// once persistence is confirmed. Returning a non-nil error tells the
// adapter to skip the ack so the broker can redeliver after AckWait.
type HandlerFunc func(ctx context.Context, d *Delivery) error

// Subscribe binds handler to subject under queue group. Each delivery
// produces one HandlerFunc invocation inside a fresh background
// context derived from ctx. The returned *natsgo.Subscription is
// owned by the caller — Drain or Unsubscribe it on shutdown.
//
// ackWait is the JetStream redelivery timeout. Pick > the slowest
// scan latency the worker should tolerate; ClamAV scans of a few MB
// fit comfortably in 30s.
func (a *SDKAdapter) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler HandlerFunc,
) (*natsgo.Subscription, error) {
	if handler == nil {
		return nil, errors.New("nats: handler is required")
	}
	if ackWait <= 0 {
		ackWait = 30 * time.Second
	}
	sub, err := a.js.QueueSubscribe(subject, queue, func(m *natsgo.Msg) {
		d := &Delivery{m: m}
		if err := handler(ctx, d); err != nil {
			// Best-effort negative ack so the broker redelivers
			// sooner than AckWait. Failure here is logged-and-
			// ignored: AckWait will redeliver regardless.
			_ = m.Nak()
		}
	},
		natsgo.Durable(durable),
		natsgo.ManualAck(),
		natsgo.AckWait(ackWait),
		natsgo.DeliverAll(),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: subscribe %q: %w", subject, err)
	}
	return sub, nil
}

// Drain closes the underlying connection gracefully — in-flight
// messages have a chance to ack before the conn drops. Call from the
// SIGTERM handler.
func (a *SDKAdapter) Drain() error {
	if a == nil || a.conn == nil {
		return nil
	}
	if err := a.conn.Drain(); err != nil {
		return fmt.Errorf("nats: drain: %w", err)
	}
	return nil
}

// Close hard-closes the underlying connection. Prefer Drain in
// graceful-shutdown paths.
func (a *SDKAdapter) Close() {
	if a == nil || a.conn == nil {
		return
	}
	a.conn.Close()
}

// Delivery adapts *natsgo.Msg to the worker.Delivery contract.
// Created by Subscribe; never constructed by callers.
type Delivery struct {
	m *natsgo.Msg
}

// Data returns the message body.
func (d *Delivery) Data() []byte {
	if d == nil || d.m == nil {
		return nil
	}
	return d.m.Data
}

// Ack signals successful processing to JetStream. Idempotent at the
// SDK layer: ack-after-ack is silently swallowed.
func (d *Delivery) Ack(_ context.Context) error {
	if d == nil || d.m == nil {
		return errors.New("nats: nil delivery")
	}
	if err := d.m.Ack(); err != nil {
		return fmt.Errorf("nats: ack: %w", err)
	}
	return nil
}
