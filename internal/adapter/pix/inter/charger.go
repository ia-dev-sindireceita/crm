package inter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/pericles-luz/crm/internal/billing/pix"
)

// DefaultTimeout caps the round-trip duration for a single Inter HTTP
// call. Inter's PIX surface responds well under 1s in normal
// conditions; 15s absorbs the long tail without stalling the caller's
// request path.
const DefaultTimeout = 15 * time.Second

// defaultScope is the OAuth2 scope set for the PIX cobrança surface.
// Least-privilege: only what /cob and the related read/write paths
// need. cobv.* is included for parity with future scheduled-cob work
// even though the current Create call uses /cob.
const defaultScope = "cob.write cob.read cobv.write cobv.read pix.read"

// Doer is the narrow http.Client surface the adapter needs. Tests
// inject an httptest.NewTLSServer's Client via WithClient so they can
// exercise the full mTLS handshake without spinning up a real Inter
// sandbox.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config holds the env-resolved Banco Inter credentials and target
// URL. The boot factory builds this from the PIX_INTER_* environment
// variables ([SIN-62958] AC: configuration via env only — never from
// a file in the repo and never from a DB row).
type Config struct {
	// ClientID is the OAuth2 client_id Inter issued for this PIX
	// integration. Required.
	ClientID string

	// ClientSecret is the OAuth2 client_secret paired with ClientID.
	// Required. Never logged.
	ClientSecret string

	// CertPath is the absolute path on disk to the PEM-encoded
	// mTLS client certificate Inter issued for this integration.
	// Required.
	CertPath string

	// KeyPath is the absolute path on disk to the matching
	// PEM-encoded private key. Required. The key file MUST be
	// readable only by the service user (mode 0400/0600); the
	// adapter does NOT enforce this — ops does.
	KeyPath string

	// BaseURL is the Inter API root, e.g.
	// https://cdpj.partners.bancointer.com.br for production or
	// https://cdpj-sandbox.partners.uatinter.co for sandbox.
	// Required, no default — refusing to default avoids accidentally
	// hitting prod from a misconfigured staging deploy.
	BaseURL string

	// Chave is the merchant's PIX key (CPF/CNPJ/email/EVP) that
	// will receive funds. Required. SaaS-global for now — the CRM
	// is the merchant, tenants are customers.
	Chave string

	// CACertPath optionally points at a PEM bundle of additional
	// root CAs for verifying Inter's server cert. Empty means use
	// the host's system roots, which is the production default.
	// Tests use this to trust the httptest.NewTLSServer cert.
	CACertPath string

	// Scope overrides the default OAuth2 scope string. Empty means
	// use defaultScope (the production value).
	Scope string
}

// Charger is the pix.PIXCharger implementation backed by Banco Inter.
// Construct with New.
type Charger struct {
	clientID     string
	clientSecret string
	baseURL      string
	chave        string
	scope        string

	client  Doer
	logger  *slog.Logger
	tracer  trace.Tracer
	now     func() time.Time
	timeout time.Duration

	tokens *tokenCache
}

// Compile-time port assertion. If the pix.PIXCharger interface drifts
// this line stops the build.
var _ pix.PIXCharger = (*Charger)(nil)

// ErrMissingConfig is returned by New when a required Config field is
// empty or the cert/key cannot be loaded.
var ErrMissingConfig = errors.New("pix.inter: missing configuration")

// ErrUpstream marks a non-2xx response from Inter (excluding token
// failures, which surface as ErrTokenRefresh). Callers may use
// errors.Is to branch on upstream availability vs. local validation.
var ErrUpstream = errors.New("pix.inter: upstream error")

// ErrStatusUnknown is returned by Status when Inter responds with a
// status string the adapter does not know how to map onto pix.Status.
// Defensive: the BACEN PIX spec is stable but we don't want to
// silently translate a future status to a guess.
var ErrStatusUnknown = errors.New("pix.inter: unknown upstream status")

// New validates cfg, loads the mTLS keypair, and returns a Charger
// ready for production traffic. Tests typically follow up with
// WithClient (to point at an httptest server) and WithLogger.
//
// Failure modes are surfaced as ErrMissingConfig with a hint about
// which field is wrong so the operator sees "PIX_INTER_CERT_PATH:
// open …: no such file or directory" rather than a generic
// "invalid config".
func New(cfg Config) (*Charger, error) {
	switch {
	case cfg.ClientID == "":
		return nil, fmt.Errorf("%w: ClientID is required", ErrMissingConfig)
	case cfg.ClientSecret == "":
		return nil, fmt.Errorf("%w: ClientSecret is required", ErrMissingConfig)
	case cfg.CertPath == "":
		return nil, fmt.Errorf("%w: CertPath is required", ErrMissingConfig)
	case cfg.KeyPath == "":
		return nil, fmt.Errorf("%w: KeyPath is required", ErrMissingConfig)
	case cfg.BaseURL == "":
		return nil, fmt.Errorf("%w: BaseURL is required", ErrMissingConfig)
	case cfg.Chave == "":
		return nil, fmt.Errorf("%w: Chave is required", ErrMissingConfig)
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: load keypair: %v", ErrMissingConfig, err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.CACertPath != "" {
		bundle, readErr := os.ReadFile(cfg.CACertPath)
		if readErr != nil {
			return nil, fmt.Errorf("%w: read CA bundle: %v", ErrMissingConfig, readErr)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("%w: CA bundle contains no PEM certificates", ErrMissingConfig)
		}
		tlsConf.RootCAs = pool
	}

	scope := cfg.Scope
	if scope == "" {
		scope = defaultScope
	}

	transport := &http.Transport{TLSClientConfig: tlsConf}
	httpClient := &http.Client{Transport: transport, Timeout: DefaultTimeout}

	return &Charger{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		chave:        cfg.Chave,
		scope:        scope,
		client:       httpClient,
		logger:       slog.Default(),
		tracer:       otel.Tracer("github.com/pericles-luz/crm/internal/adapter/pix/inter"),
		now:          time.Now,
		timeout:      DefaultTimeout,
		tokens:       &tokenCache{},
	}, nil
}

// WithLogger returns a copy of c that emits structured logs to l.
// Tests pass a discarding logger to keep output clean.
func (c *Charger) WithLogger(l *slog.Logger) *Charger {
	cp := *c
	if l != nil {
		cp.logger = l
	}
	return &cp
}

// WithNow returns a copy of c using nowFn as the clock. Token-cache
// tests use this to fast-forward past the refresh-skew boundary.
func (c *Charger) WithNow(nowFn func() time.Time) *Charger {
	cp := *c
	if nowFn != nil {
		cp.now = nowFn
	}
	return &cp
}

// cobRequest mirrors the JSON shape of PUT /cob/{txid}. Field names
// match the BACEN PIX cobrança imediata spec exactly — Inter
// implements that spec without extensions for this endpoint.
type cobRequest struct {
	Calendario         cobRequestCalendario `json:"calendario"`
	Devedor            *cobDevedor          `json:"devedor,omitempty"`
	Valor              cobValor             `json:"valor"`
	Chave              string               `json:"chave"`
	SolicitacaoPagador string               `json:"solicitacaoPagador,omitempty"`
}

type cobRequestCalendario struct {
	// Expiracao is the TTL in seconds. Inter ignores fractional
	// seconds; the adapter rounds up to keep a sub-second
	// ExpiresAt from collapsing to a zero TTL.
	Expiracao int `json:"expiracao"`
}

type cobDevedor struct {
	CPF  string `json:"cpf,omitempty"`
	CNPJ string `json:"cnpj,omitempty"`
	Nome string `json:"nome"`
}

type cobValor struct {
	// Original is the amount as "12.34" — a string with two
	// decimal places. BACEN forbids the JSON-number form because
	// some PIX participants parse it as a float and lose
	// precision.
	Original string `json:"original"`
}

// cobResponse is the JSON shape PUT /cob/{txid} and GET /cob/{txid}
// return. We only consume the fields we need; everything else is
// ignored so a future Inter additive change cannot break parsing.
type cobResponse struct {
	TxID          string `json:"txid"`
	Status        string `json:"status"`
	PixCopiaECola string `json:"pixCopiaECola"`
}

// Create issues an immediate PIX charge against Inter and returns the
// txid + EMVCo payload + base64-encoded QR PNG.
//
// txid generation is deterministic from invoice id (uuid → 32 hex
// chars, which is exactly the txid format the BACEN spec allows).
// That makes Create idempotent at the caller boundary: a retry with
// the same invoice id PUTs to the same txid and Inter returns the
// already-existing charge.
func (c *Charger) Create(ctx context.Context, req pix.ChargeRequest) (pix.ChargeResponse, error) {
	ctx, span := c.tracer.Start(ctx, "pix.inter.create", trace.WithAttributes(
		attribute.String("psp", "inter"),
	))
	defer span.End()

	if err := validateChargeRequest(req); err != nil {
		span.RecordError(err)
		return pix.ChargeResponse{}, err
	}

	txid := txidFromInvoice(req.InvoiceID)
	span.SetAttributes(attribute.String("pix.txid", txid))

	expiracao := int(req.ExpiresAt.Sub(c.now()).Round(time.Second).Seconds())
	if expiracao < 60 {
		// BACEN's cobrança imediata caps expiracao at 24h and
		// rejects values below 60s as "indistinguishable from
		// already-expired". The domain enforces ExpiresAt > now,
		// but the round-trip can shave seconds — clamp to 60s.
		expiracao = 60
	}

	body := cobRequest{
		Calendario:         cobRequestCalendario{Expiracao: expiracao},
		Valor:              cobValor{Original: formatCents(req.AmountCents)},
		Chave:              c.chave,
		SolicitacaoPagador: fmt.Sprintf("Cobranca CRM %s", req.InvoiceID.String()),
	}
	if req.PayerName != "" && req.PayerDocument != "" {
		dev := &cobDevedor{Nome: req.PayerName}
		switch len(req.PayerDocument) {
		case 11:
			dev.CPF = req.PayerDocument
		case 14:
			dev.CNPJ = req.PayerDocument
		default:
			// Anything else is malformed; surface it so the
			// invoice domain can catch the bug. The upstream
			// would 422 anyway.
			err := fmt.Errorf("%w: payer document must be 11 or 14 digits, got %d", ErrUpstream, len(req.PayerDocument))
			span.RecordError(err)
			return pix.ChargeResponse{}, err
		}
		body.Devedor = dev
	}

	payload, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return pix.ChargeResponse{}, fmt.Errorf("pix.inter: marshal create body: %w", err)
	}

	cob, err := c.callCob(ctx, http.MethodPut, "/cob/"+txid, payload)
	if err != nil {
		span.RecordError(err)
		return pix.ChargeResponse{}, err
	}

	if cob.PixCopiaECola == "" {
		err := fmt.Errorf("%w: Inter returned empty pixCopiaECola", ErrUpstream)
		span.RecordError(err)
		return pix.ChargeResponse{}, err
	}

	qrB64, err := qrEncodeBase64(cob.PixCopiaECola)
	if err != nil {
		span.RecordError(err)
		return pix.ChargeResponse{}, fmt.Errorf("pix.inter: render QR: %w", err)
	}

	externalID := cob.TxID
	if externalID == "" {
		// Inter echoes the txid we PUT to; the fallback keeps the
		// adapter robust against a missing field in a future
		// response shape.
		externalID = txid
	}

	c.logger.Info("pix.inter: charge created",
		slog.String("psp", "inter"),
		slog.String("external_id", externalID),
		slog.Int("expiracao_seconds", expiracao),
	)
	span.SetAttributes(attribute.String("pix.external_id", externalID))

	return pix.ChargeResponse{
		ExternalID: externalID,
		QRCode:     qrB64,
		CopyPaste:  cob.PixCopiaECola,
	}, nil
}

// Status queries Inter for the current status of a charge and maps
// the upstream enum onto pix.Status. ErrNotFound is reserved for
// the 404 case; ErrStatusUnknown surfaces a status string the
// adapter does not recognise.
func (c *Charger) Status(ctx context.Context, externalID string) (pix.Status, error) {
	ctx, span := c.tracer.Start(ctx, "pix.inter.status", trace.WithAttributes(
		attribute.String("psp", "inter"),
		attribute.String("pix.external_id", externalID),
	))
	defer span.End()

	if externalID == "" {
		err := fmt.Errorf("%w: externalID is required", ErrUpstream)
		span.RecordError(err)
		return "", err
	}
	if !txidPattern.MatchString(externalID) {
		// Defence-in-depth path-segment guard. Today all
		// externalIDs originate from txidFromInvoice (32 hex
		// chars) and Inter's echo of that value, so this branch
		// is unreachable from current call sites. The guard
		// stays so a future caller cannot smuggle a traversal
		// sequence or a foreign-PSP identifier into the
		// /cob/{txid} URL.
		err := fmt.Errorf("%w: externalID format", ErrUpstream)
		span.RecordError(err)
		return "", err
	}

	cob, err := c.callCob(ctx, http.MethodGet, "/cob/"+externalID, nil)
	if err != nil {
		span.RecordError(err)
		return "", err
	}

	st, err := mapStatus(cob.Status)
	if err != nil {
		span.RecordError(err)
		return "", err
	}
	span.SetAttributes(attribute.String("pix.status", string(st)))
	return st, nil
}

// callCob runs an authenticated request against Inter's /cob/* surface
// with one transparent retry on a 401 — the cache may have served a
// token that Inter rotated out from under us. callers see ErrUpstream
// on any other non-2xx.
func (c *Charger) callCob(ctx context.Context, method, path string, body []byte) (*cobResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	cob, status, err := c.cobOnce(ctx, method, path, body)
	if err == nil {
		return cob, nil
	}
	if status != http.StatusUnauthorized {
		return nil, err
	}

	// One retry after invalidating the cached token. We do NOT loop
	// further: a second 401 is a credential problem, not a stale
	// cache, and looping would burn the rate limit.
	c.tokens.mu.Lock()
	c.tokens.token = ""
	c.tokens.expiresAt = time.Time{}
	c.tokens.mu.Unlock()
	cob, _, retryErr := c.cobOnce(ctx, method, path, body)
	if retryErr != nil {
		return nil, retryErr
	}
	return cob, nil
}

// cobOnce is the single-shot request against Inter. It returns the
// decoded response (or nil), the HTTP status (for caller-side retry
// decisions), and the error. Logs only the (method, path, status)
// triple — request and response bodies are never logged.
func (c *Charger) cobOnce(ctx context.Context, method, path string, body []byte) (*cobResponse, int, error) {
	token, err := c.fetchToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("pix.inter: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("pix.inter: request failed",
			slog.String("psp", "inter"),
			slog.String("method", method),
			slog.String("path", path),
		)
		return nil, 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	c.logger.Info("pix.inter: request",
		slog.String("psp", "inter"),
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
	)

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, fmt.Errorf("%w: %w", ErrUpstream, pix.ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("%w: inter status %d", ErrUpstream, resp.StatusCode)
	}

	var decoded cobResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}
	return &decoded, resp.StatusCode, nil
}

// validateChargeRequest checks the parts of a ChargeRequest the
// adapter must enforce before it serialises the upstream body. The
// domain has its own invariants on PIXCharge construction; this
// covers the wire-format gaps so we never PUT garbage to Inter.
func validateChargeRequest(req pix.ChargeRequest) error {
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: AmountCents must be positive", ErrUpstream)
	}
	if req.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: ExpiresAt is required", ErrUpstream)
	}
	return nil
}

// formatCents renders an int64 amount in centavos as the "12.34"
// string Inter (and BACEN) expect. validateChargeRequest rejects
// non-positive amounts before this is reached.
func formatCents(cents int64) string {
	reais := cents / 100
	centavos := cents % 100
	return fmt.Sprintf("%d.%02d", reais, centavos)
}

// txidPattern is the BACEN-published txid format (cobrança imediata):
// 26–35 alphanumeric characters, no punctuation. Used to gate
// Status(externalID) so an adapter caller can never coerce arbitrary
// path segments (e.g. "../oauth/v2/token") into the /cob/{txid} URL.
var txidPattern = regexp.MustCompile(`^[A-Za-z0-9]{26,35}$`)

// txidFromInvoice derives the BACEN txid from an invoice UUID by
// stripping the hyphens. The result is exactly 32 hex chars, which
// satisfies the BACEN constraint (`^[a-zA-Z0-9]{26,35}$`).
//
// Deterministic-by-design: a retry with the same invoice id PUTs to
// the same txid and Inter responds with the existing charge instead
// of creating a duplicate. The caller (the invoice use case) gets
// idempotency for free.
func txidFromInvoice(invoiceID uuid.UUID) string {
	return strings.ReplaceAll(invoiceID.String(), "-", "")
}

// mapStatus translates Inter's status enum onto pix.Status. The four
// values come from the BACEN PIX cobrança spec; Inter does not
// extend it. Any other value is surfaced as ErrStatusUnknown so an
// upstream change is loud, not silent.
func mapStatus(s string) (pix.Status, error) {
	switch s {
	case "ATIVA":
		return pix.StatusPending, nil
	case "CONCLUIDA":
		return pix.StatusPaid, nil
	case "REMOVIDA_PELO_USUARIO_RECEBEDOR", "REMOVIDA_PELO_PSP":
		return pix.StatusCancelled, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrStatusUnknown, s)
	}
}
