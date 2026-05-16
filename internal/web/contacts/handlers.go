// Package contacts is the HTMX UI for the identity-merge surface
// (SIN-62799 / Fase 2 F2-13). It serves a single contact-detail page
// that lists every contact merged into the same Identity, and a POST
// endpoint that splits one link off into a fresh Identity when the
// auto-merge was wrong.
//
// The package is structured like internal/web/inbox: it depends only on
// use cases declared in internal/contacts/usecase plus tenancy + CSRF
// helpers. Templates are inline html/template strings so no build step
// is introduced; the forbidwebboundary lint enforces that handlers do
// not reach past the use-case layer into the contacts aggregate root.
package contacts

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// LoadIdentityForContactUseCase is the read-side dependency: returns the
// Identity (+ Links) that a contact is currently merged into.
type LoadIdentityForContactUseCase interface {
	Execute(ctx context.Context, in contactsusecase.LoadIdentityInput) (contactsusecase.LoadIdentityResult, error)
}

// SplitIdentityLinkUseCase is the write-side dependency: detaches a
// single contact_identity_link row into a fresh Identity and returns
// the survivor's post-split Identity for the HTMX fragment swap.
type SplitIdentityLinkUseCase interface {
	Execute(ctx context.Context, in contactsusecase.SplitInput) (contactsusecase.SplitResult, error)
}

// CSRFTokenFn returns the request's CSRF token. The handler treats an
// empty token as a 500 because RequireAuth guarantees a session.
type CSRFTokenFn func(*http.Request) string

// Deps bundles the handler collaborators. All fields are required.
type Deps struct {
	LoadIdentity LoadIdentityForContactUseCase
	SplitLink    SplitIdentityLinkUseCase
	CSRFToken    CSRFTokenFn
	Logger       *slog.Logger
}

// Handler serves GET /contacts/{contactID} and POST
// /contacts/identity/split. Mount with Routes.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil dependencies are rejected; the logger
// falls back to slog.Default when nil so callers in tests don't need
// to plumb it.
func New(deps Deps) (*Handler, error) {
	if deps.LoadIdentity == nil {
		return nil, errors.New("web/contacts: LoadIdentity is required")
	}
	if deps.SplitLink == nil {
		return nil, errors.New("web/contacts: SplitLink is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/contacts: CSRFToken is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the two endpoints on mux. POST /contacts/identity/split
// uses the singleton path (linkID lives in the form body) so the URL
// stays bookmarkable + the CSRF middleware can apply a single rule.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /contacts/{contactID}", h.view)
	mux.HandleFunc("POST /contacts/identity/split", h.split)
}

// view renders the full contact-identity page: one row per IdentityLink
// (one per sibling contact merged into the same Identity). Each row
// carries a "Separar este contato" button that POSTs to the split
// endpoint with the link id + the survivor contact id (the contact the
// operator is currently viewing).
func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	contactID, err := uuid.Parse(r.PathValue("contactID"))
	if err != nil {
		http.Error(w, "invalid contact id", http.StatusBadRequest)
		return
	}
	res, err := h.deps.LoadIdentity.Execute(r.Context(), contactsusecase.LoadIdentityInput{
		TenantID: tenant.ID, ContactID: contactID,
	})
	if err != nil {
		if errors.Is(err, contacts.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "load identity", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := contactLayoutTmpl.Execute(w, layoutData{
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
		Panel: panelData{
			ContactID: contactID,
			Identity:  res.Identity,
			CSRFInput: csrf.FormHidden(token),
		},
	}); err != nil {
		h.deps.Logger.Error("web/contacts: render layout", "err", err)
	}
}

// split handles the POST. Reads link_id + survivor_contact_id from the
// form, calls the use case, then renders the post-split panel as an
// HTMX fragment (hx-swap=outerHTML targets #identity-panel).
func (h *Handler) split(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	linkID, err := uuid.Parse(r.PostFormValue("link_id"))
	if err != nil {
		http.Error(w, "invalid link id", http.StatusBadRequest)
		return
	}
	survivorID, err := uuid.Parse(r.PostFormValue("survivor_contact_id"))
	if err != nil {
		http.Error(w, "invalid survivor contact id", http.StatusBadRequest)
		return
	}
	res, err := h.deps.SplitLink.Execute(r.Context(), contactsusecase.SplitInput{
		TenantID: tenant.ID, LinkID: linkID, SurvivorContactID: survivorID,
	})
	if err != nil {
		if errors.Is(err, contacts.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "split identity", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := identityPanelTmpl.Execute(w, panelData{
		ContactID: survivorID,
		Identity:  res.Identity,
		CSRFInput: csrf.FormHidden(token),
	}); err != nil {
		h.deps.Logger.Error("web/contacts: render panel", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/contacts: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// panelData drives the identity panel partial. The full-page layout
// embeds the same data so a re-render of the panel matches the initial
// GET output byte-for-byte.
type panelData struct {
	ContactID uuid.UUID
	Identity  *contacts.Identity
	CSRFInput template.HTML
}

// layoutData drives the full-page contact view.
type layoutData struct {
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
	Panel     panelData
}
