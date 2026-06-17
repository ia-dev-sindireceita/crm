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
	"strconv"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
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

// ListContactsUseCase is the read-side dependency backing the
// list/search pane (GET /contacts). Optional — see Deps.ListContacts.
type ListContactsUseCase interface {
	Execute(ctx context.Context, in contactsusecase.ListContactsInput) (contactsusecase.ListContactsResult, error)
}

// GetContactDetailUseCase enriches the contact view with channels and
// recent conversation history (GET /contacts/{contactID}). Optional —
// see Deps.GetDetail.
type GetContactDetailUseCase interface {
	Execute(ctx context.Context, in contactsusecase.GetContactDetailInput) (contactsusecase.GetContactDetailResult, error)
}

// UpdateContactUseCase backs the edit form (GET + POST
// /contacts/{contactID}/edit). Optional — see Deps.UpdateContact.
type UpdateContactUseCase interface {
	Execute(ctx context.Context, in contactsusecase.UpdateContactInput) (contactsusecase.UpdateContactResult, error)
}

// CSRFTokenFn returns the request's CSRF token. The handler treats an
// empty token as a 500 because RequireAuth guarantees a session.
type CSRFTokenFn func(*http.Request) string

// Deps bundles the handler collaborators. LoadIdentity, SplitLink and
// CSRFToken are required (New rejects nil). ListContacts, GetDetail and
// UpdateContact are the SIN-64977 management-surface use cases and are
// OPTIONAL: a deployment that only needs the SIN-62855 identity-split
// page leaves them nil, in which case the list/edit routes are not
// registered and the detail view degrades to the identity panel only.
type Deps struct {
	LoadIdentity LoadIdentityForContactUseCase
	SplitLink    SplitIdentityLinkUseCase
	CSRFToken    CSRFTokenFn
	Logger       *slog.Logger

	// ListContacts backs GET /contacts. When nil the list route is not
	// mounted.
	ListContacts ListContactsUseCase
	// GetDetail enriches GET /contacts/{contactID} with channels +
	// conversation history. When nil the view renders the identity panel
	// only (pre-SIN-64977 behaviour).
	GetDetail GetContactDetailUseCase
	// UpdateContact backs the edit form. The edit routes are mounted only
	// when BOTH UpdateContact and GetDetail are non-nil (the form prefill
	// reads the current name through GetDetail).
	UpdateContact UpdateContactUseCase
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

// Routes mounts the contacts endpoints on mux. POST
// /contacts/identity/split uses the singleton path (linkID lives in the
// form body) so the URL stays bookmarkable + the CSRF middleware can
// apply a single rule.
//
// The list (GET /contacts) and edit (GET+POST /contacts/{contactID}/edit)
// routes are registered only when their optional use cases are wired, so
// the identity-split-only deployment (SIN-62855) keeps its exact route
// table. Go 1.22 pattern precedence resolves the static /contacts and the
// /{contactID}/edit suffix ahead of the /{contactID} catch-all, so the
// three GET patterns coexist without manual ordering.
func (h *Handler) Routes(mux *http.ServeMux) {
	if h.deps.ListContacts != nil {
		mux.HandleFunc("GET /contacts", h.list)
	}
	mux.HandleFunc("GET /contacts/{contactID}", h.view)
	if h.deps.UpdateContact != nil && h.deps.GetDetail != nil {
		mux.HandleFunc("GET /contacts/{contactID}/edit", h.editForm)
		mux.HandleFunc("POST /contacts/{contactID}/edit", h.update)
	}
	mux.HandleFunc("POST /contacts/identity/split", h.split)
}

// list renders the paginated, searchable contacts table. A search box
// (hx-get, keyup-debounced) and the pager swap only the results region
// (#contacts-results); a full navigation renders the page shell. The
// HX-Request header distinguishes the two so the same handler serves
// both without a query-string flag.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	q := r.URL.Query()
	in := contactsusecase.ListContactsInput{
		TenantID: tenant.ID,
		Query:    q.Get("q"),
		Limit:    atoiOrZero(q.Get("limit")),
		Offset:   atoiOrZero(q.Get("offset")),
	}
	res, err := h.deps.ListContacts.Execute(r.Context(), in)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list contacts", err)
		return
	}
	results := buildResults(res, in.Query)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHXRequest(r) {
		if err := contactsResultsTmpl.Execute(w, results); err != nil {
			h.deps.Logger.Error("web/contacts: render results", "err", err)
		}
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	if err := contactsListTmpl.Execute(w, listLayoutData{
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		Results:          results,
	}); err != nil {
		h.deps.Logger.Error("web/contacts: render list", "err", err)
	}
}

// editForm serves the contact edit form. A full navigation renders the
// page shell; an HTMX request (e.g. the detail page "Editar" affordance)
// returns the form fragment so it can swap into #contact-edit-panel.
func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	contactID, ok := h.parseContactID(w, r)
	if !ok {
		return
	}
	res, err := h.deps.GetDetail.Execute(r.Context(), contactsusecase.GetContactDetailInput{
		TenantID: tenant.ID, ContactID: contactID,
	})
	if err != nil {
		if errors.Is(err, contacts.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get contact detail", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	form := editFormData{
		ContactID:   contactID,
		DisplayName: res.Contact.DisplayName,
		CSRFInput:   csrf.FormHidden(token),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHXRequest(r) {
		if err := contactEditPanelTmpl.Execute(w, form); err != nil {
			h.deps.Logger.Error("web/contacts: render edit fragment", "err", err)
		}
		return
	}
	if err := contactEditPageTmpl.Execute(w, editLayoutData{
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		Form:             form,
	}); err != nil {
		h.deps.Logger.Error("web/contacts: render edit page", "err", err)
	}
}

// update applies the edit. On success an HTMX caller gets the saved panel
// fragment for the #contact-edit-panel swap; a plain form post is
// redirected back to the detail page (progressive enhancement). A blank
// name re-renders the form with a 422 + inline error; a missing contact
// is a 404.
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	contactID, ok := h.parseContactID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("display_name")
	res, err := h.deps.UpdateContact.Execute(r.Context(), contactsusecase.UpdateContactInput{
		TenantID: tenant.ID, ContactID: contactID, DisplayName: name,
	})
	if err != nil {
		switch {
		case errors.Is(err, contacts.ErrNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		case errors.Is(err, contacts.ErrEmptyDisplayName):
			token := h.deps.CSRFToken(r)
			if token == "" {
				h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnprocessableEntity)
			if err := contactEditPanelTmpl.Execute(w, editFormData{
				ContactID:   contactID,
				DisplayName: name,
				CSRFInput:   csrf.FormHidden(token),
				Error:       "O nome de exibição não pode ficar em branco.",
			}); err != nil {
				h.deps.Logger.Error("web/contacts: render edit error", "err", err)
			}
		default:
			h.fail(w, http.StatusInternalServerError, "update contact", err)
		}
		return
	}
	if !isHXRequest(r) {
		http.Redirect(w, r, "/contacts/"+contactID.String(), http.StatusSeeOther)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := contactSavedPanelTmpl.Execute(w, savedPanelData{
		Contact: res.Contact,
	}); err != nil {
		h.deps.Logger.Error("web/contacts: render saved panel", "err", err)
	}
}

// parseContactID validates the {contactID} path value, writing the right
// 4xx and returning ok=false on failure. uuid.Nil is mapped to 404 (the
// SIN-63219 sentinel rule) so it joins the regular not-found path.
func (h *Handler) parseContactID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	contactID, err := uuid.Parse(r.PathValue("contactID"))
	if err != nil {
		http.Error(w, "invalid contact id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	if contactID == uuid.Nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return uuid.Nil, false
	}
	return contactID, true
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
	// uuid.Nil is syntactically valid but never identifies a real contact.
	// Map it to 404 so it joins the regular "not found" path instead of
	// tripping the use-case nil-guard and leaking a 500 — see SIN-63219.
	if contactID == uuid.Nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
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
	// SIN-64977 — enrich the page with the contact's editable header,
	// channel set and recent conversation history when the detail use
	// case is wired. A nil GetDetail (identity-split-only deployment)
	// degrades to the pre-SIN-64977 identity panel.
	var detail *contactsusecase.ContactDetailView
	if h.deps.GetDetail != nil {
		dres, derr := h.deps.GetDetail.Execute(r.Context(), contactsusecase.GetContactDetailInput{
			TenantID: tenant.ID, ContactID: contactID,
		})
		if derr != nil {
			if errors.Is(derr, contacts.ErrNotFound) {
				http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
				return
			}
			h.fail(w, http.StatusInternalServerError, "get contact detail", derr)
			return
		}
		detail = &dres.Contact
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := contactLayoutTmpl.Execute(w, layoutData{
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		Detail:           detail,
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
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	Panel            panelData
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275).
	CSPNonce string
	// Detail carries the SIN-64977 enrichment (channels + conversation
	// history + edit affordance). Nil when GetDetail is not wired, in
	// which case the detail block is omitted entirely.
	Detail *contactsusecase.ContactDetailView
}

// resultsData drives the contacts list results region (#contacts-results):
// the rows plus the derived pager state. It is the swap unit for search
// and pagination so both reuse one template.
type resultsData struct {
	Query   string
	Items   []contactsusecase.ContactSummaryView
	Total   int
	Limit   int
	Offset  int
	HasPrev bool
	HasNext bool
	PrevOff int
	NextOff int
	// From/To are the 1-based bounds of the current page ("N–M de Total").
	From int
	To   int
}

// listLayoutData drives the full-page contacts list shell.
type listLayoutData struct {
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	CSPNonce         string
	Results          resultsData
}

// editFormData drives the edit form fragment (#contact-edit-panel). Error
// is non-empty only on a re-render after a validation failure.
type editFormData struct {
	ContactID   uuid.UUID
	DisplayName string
	CSRFInput   template.HTML
	Error       string
}

// editLayoutData drives the full-page edit shell.
type editLayoutData struct {
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	CSPNonce         string
	Form             editFormData
}

// savedPanelData drives the post-save panel (#contact-edit-panel) that
// replaces the form on a successful HTMX update.
type savedPanelData struct {
	Contact contactsusecase.ContactSummaryView
}

// buildResults derives the pager state from a ListContacts result.
func buildResults(res contactsusecase.ListContactsResult, query string) resultsData {
	rd := resultsData{
		Query:  query,
		Items:  res.Items,
		Total:  res.Total,
		Limit:  res.Limit,
		Offset: res.Offset,
	}
	if res.Limit > 0 {
		rd.HasPrev = res.Offset > 0
		rd.PrevOff = res.Offset - res.Limit
		if rd.PrevOff < 0 {
			rd.PrevOff = 0
		}
		rd.HasNext = res.Offset+res.Limit < res.Total
		rd.NextOff = res.Offset + res.Limit
	}
	if len(res.Items) > 0 {
		rd.From = res.Offset + 1
		rd.To = res.Offset + len(res.Items)
	}
	return rd
}

// atoiOrZero parses a non-negative pagination param, returning 0 (which
// the use case normalises to its defaults) for blank or malformed input
// so a hostile query string can never wedge the list.
func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isHXRequest reports whether the request was issued by HTMX (the
// HX-Request header), so the handler can return a fragment instead of the
// full page shell.
func isHXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
