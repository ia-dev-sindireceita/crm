package slugreservation

import (
	"net/http"
	"strings"
)

// RedirectHandler resolves a request whose Host is `<old>.<primary>`
// to a 301 toward `<new>.<primary>` with `Clear-Site-Data: "cookies"`.
// Anything we cannot resolve falls through to next so the rest of the
// catch-all routing still runs.
//
// PrimaryHost is the apex (e.g. "crm.pitho.app"). The handler
// extracts the leftmost label as the candidate slug.
type RedirectHandler struct {
	svc         *Service
	primaryHost string
	next        http.Handler
}

// NewRedirectHandler builds the handler. primaryHost is required.
// next is what we delegate to when there is no matching redirect — pass
// http.NotFoundHandler() for a strict catch-all, or your real router
// to layer the redirect on top of normal routing.
func NewRedirectHandler(svc *Service, primaryHost string, next http.Handler) *RedirectHandler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return &RedirectHandler{svc: svc, primaryHost: primaryHost, next: next}
}

// ServeHTTP implements http.Handler.
func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(stripPort(r.Host))
	primary := strings.ToLower(strings.TrimSpace(h.primaryHost))
	if primary == "" || host == "" {
		h.next.ServeHTTP(w, r)
		return
	}
	suffix := "." + primary
	if host == primary || !strings.HasSuffix(host, suffix) {
		h.next.ServeHTTP(w, r)
		return
	}
	slug := strings.TrimSuffix(host, suffix)
	// Disallow nested labels — only a single-label subdomain redirects.
	if slug == "" || strings.Contains(slug, ".") {
		h.next.ServeHTTP(w, r)
		return
	}

	red, err := h.svc.LookupRedirect(r.Context(), slug)
	if err != nil {
		// Invalid or no match — defer to the next handler. We do not
		// 404 here so non-redirect routes keep working.
		h.next.ServeHTTP(w, r)
		return
	}

	target := buildRedirectURL(r, red.NewSlug, primary)
	w.Header().Set("Clear-Site-Data", ClearSiteDataCookies)
	w.Header().Set("Location", target)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMovedPermanently)
}

func stripPort(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

func buildRedirectURL(r *http.Request, newSlug, primary string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		// Local/dev plaintext fallback; preserves the request scheme so
		// tests and curl-from-localhost still work.
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
		scheme = proto
	}
	path := r.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	return scheme + "://" + newSlug + "." + primary + path
}
