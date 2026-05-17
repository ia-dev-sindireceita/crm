// Package dunning is the HTTP gate that enforces the dunning state
// at the request boundary, complementing the HTMX banner in
// internal/web/billing/invoices (C12).
//
// Two block bands:
//
//   - suspended_outbound: blocks routes the operator opts into as
//     "outbound" (channel sends + LLM automations). Reads and inbound
//     paths continue. This is least-privilege gating — message tasks
//     that consume paid channel/LLM tokens stop while the tenant can
//     still receive replies and consult history.
//   - suspended_full: blocks every mutation (POST/PUT/PATCH/DELETE);
//     reads (GET/HEAD/OPTIONS) continue.
//
// Cancelled subscriptions are also rejected with the suspended_full
// rule — there is nothing to bill against, every write would be
// orphaned.
//
// The middleware is deny-by-default at the route level: only routes
// the operator explicitly tagged as "outbound" are gated for the
// suspended_outbound band. The full-suspension gate is method-shaped
// and needs no opt-in.
//
// Body messages are PT-BR per AC#4. The middleware returns
// `text/plain; charset=utf-8` for non-HTMX requests and a tiny HTMX
// fragment (`<div role="alert">…</div>`) for HTMX requests so the
// fragment merges cleanly into the page.
package dunning

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// StateLookup returns the dunning state for the request's tenant. nil
// rows surface as billingdunning.ErrNotFound; the middleware treats
// that as "current" (a tenant without a dunning row is unrestricted).
type StateLookup interface {
	CurrentForTenant(ctx context.Context, tenantID uuid.UUID) (*billingdunning.DunningState, error)
}

// OutboundClassifier reports whether a request is an "outbound" route
// (channel send, automation, etc.) that must be gated in the
// suspended_outbound band. The default classifier checks a path-prefix
// list supplied at construction time.
type OutboundClassifier func(*http.Request) bool

// Config bundles middleware dependencies.
type Config struct {
	// Lookup resolves the request's tenant to its dunning state.
	Lookup StateLookup
	// OutboundRoutes flags requests as outbound. nil means "no outbound
	// path is gated by the suspended_outbound band" — the middleware
	// still enforces suspended_full.
	OutboundRoutes OutboundClassifier
	// Logger captures denials. Defaults to slog.Default().
	Logger *slog.Logger
	// FailOpen controls behaviour when the lookup errors. Defaults to
	// false: deny the request with a 503-equivalent so a broken lookup
	// cannot accidentally lift the gate. Set to true in tests / staging
	// where availability matters more than enforcement.
	FailOpen bool
}

// Middleware wraps next with the dunning gate. Routes that have not
// been mounted under TenantScope (no tenant in context) pass through
// without gating: the middleware does not enforce on unauthenticated
// surfaces (login, /health, etc.).
type Middleware struct {
	lookup   StateLookup
	outbound OutboundClassifier
	logger   *slog.Logger
	failOpen bool
}

// New constructs the middleware. Lookup is required.
func New(cfg Config) (*Middleware, error) {
	if cfg.Lookup == nil {
		return nil, errors.New("dunning: Lookup is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.OutboundRoutes == nil {
		cfg.OutboundRoutes = neverOutbound
	}
	return &Middleware{
		lookup:   cfg.Lookup,
		outbound: cfg.OutboundRoutes,
		logger:   cfg.Logger,
		failOpen: cfg.FailOpen,
	}, nil
}

// Wrap returns an http.Handler that applies the gate before next.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			// Unauthenticated / no-tenant surface — pass through.
			next.ServeHTTP(w, r)
			return
		}
		state, err := m.lookup.CurrentForTenant(r.Context(), tenant.ID)
		if err != nil {
			if errors.Is(err, billingdunning.ErrNotFound) {
				next.ServeHTTP(w, r)
				return
			}
			m.logger.Error("dunning.middleware: lookup failed",
				slog.String("err", err.Error()),
				slog.String("tenant_id", tenant.ID.String()),
			)
			if m.failOpen {
				next.ServeHTTP(w, r)
				return
			}
			m.deny(w, r, http.StatusServiceUnavailable,
				"Não foi possível verificar o status da sua assinatura. Tente novamente em instantes.")
			return
		}
		if state == nil {
			next.ServeHTTP(w, r)
			return
		}
		decision, reason := m.classify(r, state.State())
		if decision == http.StatusOK {
			next.ServeHTTP(w, r)
			return
		}
		m.logger.Info("dunning.middleware: blocked",
			slog.String("tenant_id", tenant.ID.String()),
			slog.String("state", string(state.State())),
			slog.String("path", r.URL.Path),
			slog.String("method", r.Method),
		)
		m.deny(w, r, decision, reason)
	})
}

// classify returns the HTTP status to enforce and the PT-BR explanation
// to surface, or http.StatusOK when the request passes the gate.
//
// Rules:
//
//   - current / warn:      always allow.
//   - suspended_outbound:  block when m.outbound classifies the request
//     as outbound; everything else (reads, inbound) allowed.
//   - suspended_full:      block every mutation; reads allowed.
//   - cancelled:           block every mutation; reads allowed.
//
// Reason strings are tenant-facing PT-BR per AC#4.
func (m *Middleware) classify(r *http.Request, state billingdunning.State) (int, string) {
	switch state {
	case billingdunning.StateCurrent, billingdunning.StateWarn:
		return http.StatusOK, ""
	case billingdunning.StateSuspendedOutbound:
		if m.outbound(r) {
			return http.StatusForbidden,
				"Sua assinatura está com pagamento em atraso. " +
					"O envio de mensagens e automações ficou suspenso até a regularização."
		}
		return http.StatusOK, ""
	case billingdunning.StateSuspendedFull:
		if isReadMethod(r.Method) {
			return http.StatusOK, ""
		}
		return http.StatusForbidden,
			"Sua assinatura está com pagamento em atraso há mais de 30 dias. " +
				"A conta está em modo somente leitura até a regularização."
	case billingdunning.StateCancelled:
		if isReadMethod(r.Method) {
			return http.StatusOK, ""
		}
		return http.StatusForbidden,
			"Sua assinatura foi cancelada por falta de pagamento. " +
				"Para reativar, entre em contato com o suporte."
	}
	// Unknown state — conservative fallback: block writes, allow reads.
	if isReadMethod(r.Method) {
		return http.StatusOK, ""
	}
	return http.StatusForbidden,
		"Sua assinatura está em um estado inválido. Entre em contato com o suporte."
}

// deny writes a tenant-facing PT-BR explanation. For HTMX requests we
// emit a small fragment so HTMX swaps cleanly; for everything else we
// emit text/plain.
func (m *Middleware) deny(w http.ResponseWriter, r *http.Request, status int, reason string) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		// Minimal fragment; htmx fills the target.
		_, _ = w.Write([]byte(`<div class="dunning-blocked" role="alert">` + htmlEscape(reason) + `</div>`))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(reason))
}

// PrefixOutboundClassifier returns an OutboundClassifier that flags any
// request whose path starts with one of the given prefixes. The list
// is normalised to use leading-slash form; a request matches when
// strings.HasPrefix(r.URL.Path, prefix) is true.
//
// Empty list means "no outbound path is gated" (same effect as
// neverOutbound). Comparison is case-sensitive — URL paths are.
func PrefixOutboundClassifier(prefixes []string) OutboundClassifier {
	if len(prefixes) == 0 {
		return neverOutbound
	}
	clean := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 {
		return neverOutbound
	}
	return func(r *http.Request) bool {
		for _, p := range clean {
			if strings.HasPrefix(r.URL.Path, p) {
				return true
			}
		}
		return false
	}
}

func neverOutbound(*http.Request) bool { return false }

func isReadMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func isHTMXRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

// htmlEscape is a tiny escaper for the deny fragment. We control every
// byte of the reason string but defense-in-depth: never trust your
// own data in a generated response body.
func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}
