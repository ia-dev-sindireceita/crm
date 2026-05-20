// Package csp provides an HTTP middleware that emits a strict
// Content-Security-Policy header with a per-request nonce.
//
// SIN-62237 / F29 — derived from ADR 0077 §3.5. Every response handed to
// the wrapped handler carries a fresh 16-byte cryptographic nonce in the
// CSP header; templates read that nonce from request context via Nonce
// and emit it as the `nonce` attribute on every <script> / <style> they
// own. The policy never includes 'unsafe-inline'.
//
// Directives: default-src, script-src, style-src, object-src, base-uri,
// font-src, frame-ancestors. font-src allows 'self' and data: URIs so
// browser extensions (password managers, accessibility helpers) that
// inject inline UI with embedded woff2 fonts do not flood the console
// with CSP violation noise; see HeaderTemplate for the rationale.
//
// Design:
//
//   - The header template embeds the literal substring "{nonce}" wherever
//     the policy needs the per-request value. Every emission replaces all
//     occurrences in one pass; the literal token never reaches the client.
//   - 16 bytes from crypto/rand are encoded with base64.RawURLEncoding,
//     yielding 22 ASCII chars matching `[A-Za-z0-9_-]`. CSP3 §6.6.4
//     accepts this charset for nonce-source.
//   - Nonce is stashed in r.Context() under an unexported key. Templates
//     read it via Nonce; the empty string is returned when called outside
//     an instrumented request, which means a missing middleware fails
//     closed (the browser sees `nonce=""` and never matches the directive
//     — there is no silent escalation to unattributed inline content).
//   - crypto/rand failure → 500 + the wrapped handler is NOT invoked.
//     This is the seam that the test suite drives to prove the failure
//     branch.
package csp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
)

// HeaderName is the response header that carries the policy.
const HeaderName = "Content-Security-Policy"

// HeaderTemplate is the canonical F29 policy. The literal {nonce}
// placeholder is substituted before WriteHeader; the literal token never
// reaches a browser. Directives are joined with "; " and the order is
// stable so test assertions can match on byte equality.
const HeaderTemplate = "default-src 'self'; " +
	"script-src 'self' 'nonce-{nonce}'; " +
	"style-src 'self' 'nonce-{nonce}'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	// font-src 'self' data: silences console noise from browser-extension UI (password managers, Grammarly, etc.) that embeds woff2 fonts as data: URIs; fonts are inert binary parsers and anti-XSS/anti-clickjacking coverage is unchanged. SIN-63155.
	"font-src 'self' data:; " +
	"frame-ancestors 'none'"

// noncePlaceholder is the substring substituted with the per-request
// value. It is exported as a constant in the test file so callers can
// assert that no instance leaks past the middleware.
const noncePlaceholder = "{nonce}"

// nonceBytes is the entropy in the per-request nonce. 16 bytes encodes
// to 22 base64-url chars (no padding) — well above the CSP3 minimum
// recommendation of 128 bits and short enough to keep the header
// compact.
const nonceBytes = 16

// ctxKey is an unexported key type. Using a custom type prevents
// collisions with other packages' context values and stops external
// callers from injecting their own nonces by setting context values
// with a string key.
type ctxKey struct{}

// Middleware wraps next so every response carries a fresh CSP header
// and the request context exposes the per-request nonce via Nonce.
//
// The crypto/rand failure path returns 500 and skips next. A failure
// here is operationally severe (no entropy) and indicates the host is
// misconfigured; serving an unprotected response would be worse than
// erroring.
func Middleware(next http.Handler) http.Handler {
	return middlewareWith(next, rand.Reader)
}

// middlewareWith is the test seam. Production passes rand.Reader; tests
// pass deterministic readers (and an io.ErrUnexpectedEOF source for the
// failure branch).
func middlewareWith(next http.Handler, src io.Reader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := generateNonce(src)
		if err != nil {
			http.Error(w, "csp: failed to generate nonce", http.StatusInternalServerError)
			return
		}
		w.Header().Set(HeaderName, strings.ReplaceAll(HeaderTemplate, noncePlaceholder, nonce))
		ctx := context.WithValue(r.Context(), ctxKey{}, nonce)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateNonce reads nonceBytes from src and returns the base64-url
// no-pad encoding. io.ReadFull is used so a short read surfaces as an
// error (rand.Reader normally never short-reads, but the test seam can).
func generateNonce(src io.Reader) (string, error) {
	buf := make([]byte, nonceBytes)
	if _, err := io.ReadFull(src, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Nonce returns the per-request CSP nonce stashed by Middleware. It
// returns the empty string when called outside an instrumented request,
// so templates that emit `nonce="{{ csp.Nonce ... }}"` fail closed: the
// browser sees `nonce=""`, which never matches a CSP directive — the
// inline element is then blocked instead of silently allowed.
func Nonce(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
