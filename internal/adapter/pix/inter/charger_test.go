package inter_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/pix/inter"
	"github.com/pericles-luz/crm/internal/billing/pix"
)

// === test infrastructure ===

// mtlsHarness is everything a test needs to drive the Inter adapter
// through a real mTLS handshake against a local httptest server: the
// CA pool, the server's URL, the client cert paths on disk, and a
// per-request hook for shaping responses.
type mtlsHarness struct {
	server         *httptest.Server
	caBundlePath   string
	clientCertPath string
	clientKeyPath  string

	// handler is invoked for every request. Tests assign per-test
	// behaviour; the default (set by newMTLSHarness) responds with
	// 404 so a missing assignment is loud.
	handler atomic.Value // func(http.ResponseWriter, *http.Request)
}

func newMTLSHarness(t *testing.T) *mtlsHarness {
	t.Helper()

	caKey, caCert, caPEM := mintSelfSignedCA(t)
	serverCertPEM, serverKeyPEM := mintSignedCert(t, caCert, caKey, "127.0.0.1", true)
	clientCertPEM, clientKeyPEM := mintSignedCert(t, caCert, caKey, "inter-client", false)

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	clientCertPath := filepath.Join(dir, "client.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	clientKeyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	serverTLS, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append ca to client CAs failed")
	}

	h := &mtlsHarness{
		caBundlePath:   caPath,
		clientCertPath: clientCertPath,
		clientKeyPath:  clientKeyPath,
	}
	h.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no handler registered", http.StatusNotFound)
	}))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn := h.handler.Load().(http.HandlerFunc)
		fn(w, r)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverTLS},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.server = srv
	return h
}

func (h *mtlsHarness) setHandler(fn http.HandlerFunc) {
	h.handler.Store(fn)
}

// newCharger constructs a Charger pointed at the harness, loading the
// keypair through New() so the production mTLS code path is the one
// under test (rather than swapping the http.Client in retroactively).
func (h *mtlsHarness) newCharger(t *testing.T) *inter.Charger {
	t.Helper()
	c, err := inter.New(inter.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		CertPath:     h.clientCertPath,
		KeyPath:      h.clientKeyPath,
		BaseURL:      h.server.URL,
		Chave:        "merchant@sindireceita.test",
		CACertPath:   h.caBundlePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// === cert helpers ===

func mintSelfSignedCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "inter-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca create: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ca parse: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, cert, pemBytes
}

func mintSignedCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// === New() validation ===

func TestNew_MissingFields(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	base := inter.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		CertPath:     h.clientCertPath,
		KeyPath:      h.clientKeyPath,
		BaseURL:      h.server.URL,
		Chave:        "chave",
		CACertPath:   h.caBundlePath,
	}
	cases := []struct {
		name  string
		patch func(*inter.Config)
	}{
		{"client id", func(c *inter.Config) { c.ClientID = "" }},
		{"client secret", func(c *inter.Config) { c.ClientSecret = "" }},
		{"cert path", func(c *inter.Config) { c.CertPath = "" }},
		{"key path", func(c *inter.Config) { c.KeyPath = "" }},
		{"base url", func(c *inter.Config) { c.BaseURL = "" }},
		{"chave", func(c *inter.Config) { c.Chave = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.patch(&cfg)
			_, err := inter.New(cfg)
			if !errors.Is(err, inter.ErrMissingConfig) {
				t.Fatalf("want ErrMissingConfig, got %v", err)
			}
		})
	}
}

func TestNew_BadCertPath(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	_, err := inter.New(inter.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		CertPath:     "/nonexistent/cert.pem",
		KeyPath:      h.clientKeyPath,
		BaseURL:      h.server.URL,
		Chave:        "chave",
	})
	if !errors.Is(err, inter.ErrMissingConfig) {
		t.Fatalf("want ErrMissingConfig, got %v", err)
	}
}

func TestNew_BadCABundle(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := inter.New(inter.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		CertPath:     h.clientCertPath,
		KeyPath:      h.clientKeyPath,
		BaseURL:      h.server.URL,
		Chave:        "chave",
		CACertPath:   junk,
	})
	if !errors.Is(err, inter.ErrMissingConfig) {
		t.Fatalf("want ErrMissingConfig, got %v", err)
	}
}

// === mTLS handshake ===

func TestMTLSHandshake_ServerVerifiesClientCert(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	var seenCN string
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			seenCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "tok",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid":          "abc",
			"status":        "ATIVA",
			"pixCopiaECola": "00020126360014BR.GOV.BCB.PIX0114+5511999998888520400005303986540510.005802BR5913FULANO6008BRASILIA62070503***6304ABCD",
		})
	})

	resp, err := c.Create(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if seenCN != "inter-client" {
		t.Fatalf("server saw client CN %q, want inter-client", seenCN)
	}
	if resp.ExternalID == "" {
		t.Fatalf("empty externalID")
	}
}

func TestMTLSHandshake_MissingClientCertIsRejected(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)

	// Build a *plain* TLS client that trusts our test CA but
	// presents no client cert. Inter (and our harness) MUST refuse.
	caPEM, err := os.ReadFile(h.caBundlePath)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	plain := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
	resp, err := plain.Get(h.server.URL + "/oauth/v2/token")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("plain client succeeded; mTLS not enforced")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") {
		t.Fatalf("unexpected error %q", err)
	}
}

// === OAuth token caching ===

func TestToken_CachedAcrossCalls(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	var tokenCalls int32
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			atomic.AddInt32(&tokenCalls, 1)
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "tok-1",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid":          "abc",
			"status":        "ATIVA",
			"pixCopiaECola": "PIX-PAYLOAD",
		})
	})

	for i := 0; i < 3; i++ {
		if _, err := c.Create(context.Background(), validRequest()); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (caching)", got)
	}
}

func TestToken_RefreshesBeforeExpiry(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)

	var tokenCalls int32
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			n := atomic.AddInt32(&tokenCalls, 1)
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": fmt.Sprintf("tok-%d", n),
				"token_type":   "Bearer",
				"expires_in":   120, // 2 minutes
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid":          "abc",
			"status":        "ATIVA",
			"pixCopiaECola": "PIX",
		})
	})

	t0 := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := t0
	c := h.newCharger(t).WithNow(func() time.Time { return now })

	if _, err := c.Create(context.Background(), validRequest()); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// 30s later — well within the cached TTL, no refresh.
	now = t0.Add(30 * time.Second)
	if _, err := c.Create(context.Background(), validRequest()); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token endpoint hit %d times at 30s, want 1", got)
	}

	// 70s later — inside the 60s refresh-skew window, MUST refresh.
	now = t0.Add(70 * time.Second)
	if _, err := c.Create(context.Background(), validRequest()); err != nil {
		t.Fatalf("third Create: %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 2 {
		t.Fatalf("token endpoint hit %d times at 70s, want 2 (refresh-skew triggered)", got)
	}
}

func TestToken_RefreshAfter401(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	var tokenCalls int32
	var cobCalls int32
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			atomic.AddInt32(&tokenCalls, 1)
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "tok",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		n := atomic.AddInt32(&cobCalls, 1)
		if n == 1 {
			// First /cob call: pretend the cached token is stale.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid":          "abc",
			"status":        "ATIVA",
			"pixCopiaECola": "PIX",
		})
	})

	if _, err := c.Create(context.Background(), validRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 2 {
		t.Fatalf("token calls = %d, want 2 (initial + 401-recovery)", got)
	}
	if got := atomic.LoadInt32(&cobCalls); got != 2 {
		t.Fatalf("cob calls = %d, want 2 (failed + retry)", got)
	}
}

func TestToken_RefreshFailsOn4xx(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			http.Error(w, "bad credentials", http.StatusBadRequest)
			return
		}
		t.Fatalf("unexpected request: %s", r.URL.Path)
	})

	_, err := c.Create(context.Background(), validRequest())
	if !errors.Is(err, inter.ErrTokenRefresh) {
		t.Fatalf("want ErrTokenRefresh, got %v", err)
	}
}

// === Create response parsing ===

func TestCreate_PropagatesPSPFields(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	const sampleEMV = "00020126360014BR.GOV.BCB.PIX0114+5511999998888520400005303986540510.005802BR5913FULANO6008BRASILIA62070503***6304ABCD"
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/token":
			writeJSON(w, http.StatusOK, tokenOK())
		default:
			if r.Method != http.MethodPut {
				t.Errorf("want PUT, got %s", r.Method)
			}
			var got map[string]any
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			if got["chave"] != "merchant@sindireceita.test" {
				t.Errorf("chave = %v", got["chave"])
			}
			if val, _ := got["valor"].(map[string]any); val["original"] != "12.34" {
				t.Errorf("valor.original = %v", val["original"])
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"txid":          "tx-from-server",
				"status":        "ATIVA",
				"pixCopiaECola": sampleEMV,
			})
		}
	})

	resp, err := c.Create(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.ExternalID != "tx-from-server" {
		t.Fatalf("externalID = %q, want tx-from-server", resp.ExternalID)
	}
	if resp.CopyPaste != sampleEMV {
		t.Fatalf("CopyPaste mismatch:\n got=%q\nwant=%q", resp.CopyPaste, sampleEMV)
	}
	if resp.QRCode == "" {
		t.Fatalf("QRCode is empty — expected rendered PNG base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.QRCode)
	if err != nil {
		t.Fatalf("QRCode is not valid base64: %v", err)
	}
	if !bytes.HasPrefix(decoded, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("QRCode decoded payload is not a PNG (first bytes %x)", decoded[:8])
	}
}

func TestCreate_RejectsBadAmount(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	req := validRequest()
	req.AmountCents = 0
	_, err := c.Create(context.Background(), req)
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

func TestCreate_RejectsZeroExpiresAt(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	req := validRequest()
	req.ExpiresAt = time.Time{}
	_, err := c.Create(context.Background(), req)
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

func TestCreate_PayerDocumentRouting(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)

	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"cpf 11 digits", "12345678901", "cpf"},
		{"cnpj 14 digits", "12345678000190", "cnpj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenField string
			h.setHandler(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/v2/token" {
					writeJSON(w, http.StatusOK, tokenOK())
					return
				}
				body, _ := io.ReadAll(r.Body)
				var got map[string]any
				_ = json.Unmarshal(body, &got)
				dev, _ := got["devedor"].(map[string]any)
				if _, ok := dev["cpf"]; ok {
					seenField = "cpf"
				} else if _, ok := dev["cnpj"]; ok {
					seenField = "cnpj"
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"txid": "x", "status": "ATIVA", "pixCopiaECola": "P",
				})
			})

			req := validRequest()
			req.PayerName = "Fulano"
			req.PayerDocument = tc.doc
			_, err := c.Create(context.Background(), req)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if seenField != tc.want {
				t.Fatalf("server saw devedor.%s, want devedor.%s", seenField, tc.want)
			}
		})
	}
}

func TestCreate_RejectsMalformedPayerDocument(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, tokenOK())
			return
		}
		t.Fatalf("PUT /cob was called for a malformed payer doc; should have rejected client-side")
	})
	req := validRequest()
	req.PayerName = "X"
	req.PayerDocument = "12345" // neither 11 nor 14
	_, err := c.Create(context.Background(), req)
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

func TestCreate_UpstreamErrorBubbles(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, tokenOK())
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := c.Create(context.Background(), validRequest())
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

func TestCreate_EmptyPixCopiaECola(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, tokenOK())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid": "x", "status": "ATIVA", "pixCopiaECola": "",
		})
	})
	_, err := c.Create(context.Background(), validRequest())
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

// === Status response mapping ===

func TestStatus_MapsUpstreamEnum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		upstream string
		want     pix.Status
		wantErr  error
	}{
		{"ATIVA", pix.StatusPending, nil},
		{"CONCLUIDA", pix.StatusPaid, nil},
		{"REMOVIDA_PELO_USUARIO_RECEBEDOR", pix.StatusCancelled, nil},
		{"REMOVIDA_PELO_PSP", pix.StatusCancelled, nil},
		{"WAT", "", inter.ErrStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.upstream, func(t *testing.T) {
			h := newMTLSHarness(t)
			c := h.newCharger(t)
			h.setHandler(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/v2/token" {
					writeJSON(w, http.StatusOK, tokenOK())
					return
				}
				if r.Method != http.MethodGet {
					t.Errorf("want GET, got %s", r.Method)
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"txid": "x", "status": tc.upstream, "pixCopiaECola": "P",
				})
			})
			got, err := c.Status(context.Background(), "x")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want=%v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got != tc.want {
				t.Fatalf("status=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestStatus_NotFound(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, tokenOK())
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	_, err := c.Status(context.Background(), "unknown")
	if !errors.Is(err, pix.ErrNotFound) {
		t.Fatalf("want pix.ErrNotFound, got %v", err)
	}
}

func TestStatus_RejectsEmptyExternalID(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)
	c := h.newCharger(t)
	_, err := c.Status(context.Background(), "")
	if !errors.Is(err, inter.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}

// === log scrubbing ===

func TestLogs_NoSecrets(t *testing.T) {
	t.Parallel()
	h := newMTLSHarness(t)

	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := inter.New(inter.Config{
		ClientID:     "test-client",
		ClientSecret: "TOP-SECRET-DO-NOT-LEAK",
		CertPath:     h.clientCertPath,
		KeyPath:      h.clientKeyPath,
		BaseURL:      h.server.URL,
		Chave:        "merchant@sindireceita.test",
		CACertPath:   h.caBundlePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c = c.WithLogger(lg)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "BEARER-TOKEN-SHOULD-NOT-LEAK",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"txid": "x", "status": "ATIVA", "pixCopiaECola": "PIX",
		})
	})
	if _, err := c.Create(context.Background(), validRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	out := buf.String()
	for _, secret := range []string{"TOP-SECRET-DO-NOT-LEAK", "BEARER-TOKEN-SHOULD-NOT-LEAK"} {
		if strings.Contains(out, secret) {
			t.Errorf("log output contained secret %q\nlog:\n%s", secret, out)
		}
	}
}

// === helpers ===

func validRequest() pix.ChargeRequest {
	return pix.ChargeRequest{
		TenantID:    uuid.New(),
		InvoiceID:   uuid.New(),
		AmountCents: 1234,
		ExpiresAt:   time.Now().Add(30 * time.Minute),
	}
}

func tokenOK() map[string]any {
	return map[string]any{
		"access_token": "tok",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
