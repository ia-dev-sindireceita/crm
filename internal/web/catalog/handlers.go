package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/catalog"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// MaxNameLen caps the Product.Name input to keep a runaway client from
// stuffing arbitrary-length strings into the product.name text column.
// The column itself is text without a length cap; this gate is defense
// in depth at the UI boundary.
const MaxNameLen = 200

// MaxDescriptionLen caps Product.Description. The product description
// is rendered into the HTMX detail page and can show up in resolver
// previews, so an unbounded blob would make the page unrenderable.
const MaxDescriptionLen = 2000

// MaxTagLen / MaxTags cap each tag and the total number of tags so a
// runaway tag list doesn't bloat the row or the future GIN index the
// W2B resolver may add.
const (
	MaxTagLen = 64
	MaxTags   = 32
)

// MaxScopeIDLen caps the free-form scope_id input. The migration stores
// scope_id as text without a length cap; same defense in depth as
// web/aipolicy.
const MaxScopeIDLen = 128

// MaxArgumentTextLen caps ProductArgument.Text. The text is rendered
// into the IA prompt downstream (W4D) so the cap mirrors a reasonable
// prompt-chunk size.
const MaxArgumentTextLen = 4000

// MaxPriceCents is the upper bound the form accepts. The migration
// only requires price_cents >= 0; this gate keeps obviously broken
// inputs (e.g. "abc" → very large parsed values, JS-injected numbers)
// from reaching the database.
const MaxPriceCents = 1_000_000_000

// ProductReader is the read-side dependency the page consumes.
//
// The handler depends on the narrow read surface so tests do not have
// to stub write methods they never exercise.
type ProductReader interface {
	GetByID(ctx context.Context, tenantID, productID uuid.UUID) (*catalog.Product, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*catalog.Product, error)
}

// ProductWriter is the write-side dependency. SaveProduct is upsert; the
// catalog.NewProduct constructor's invariants already gated the row, so
// the adapter just needs to write.
type ProductWriter interface {
	SaveProduct(ctx context.Context, p *catalog.Product, actorID uuid.UUID) error
	DeleteProduct(ctx context.Context, tenantID, productID, actorID uuid.UUID) error
}

// ArgumentReader is the read-side dependency for the per-product
// argument list and the cascade preview.
type ArgumentReader interface {
	ListByProduct(ctx context.Context, tenantID, productID uuid.UUID) ([]*catalog.ProductArgument, error)
}

// ArgumentWriter is the write-side dependency for arguments.
type ArgumentWriter interface {
	SaveArgument(ctx context.Context, a *catalog.ProductArgument, actorID uuid.UUID) error
	DeleteArgument(ctx context.Context, tenantID, argumentID, actorID uuid.UUID) error
}

// ArgumentResolver is the cascade-preview port. The concrete
// *catalog.Resolver satisfies it; tests substitute an in-memory stub.
type ArgumentResolver interface {
	ResolveArguments(ctx context.Context, tenantID, productID uuid.UUID, scope catalog.Scope) ([]*catalog.ProductArgument, error)
}

// CSRFTokenFn returns the request's CSRF token. The handler treats an
// empty token as a 500 because RequireAuth guarantees a session.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id used as the actor on every
// write. uuid.Nil rejects the mutation with 401-equivalent because the
// repository contract requires a non-nil actor for the audit trail.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler collaborators. Every port is required;
// Now and Logger default to time.Now (UTC) and slog.Default.
type Deps struct {
	ProductReader  ProductReader
	ProductWriter  ProductWriter
	ArgumentReader ArgumentReader
	ArgumentWriter ArgumentWriter
	Resolver       ArgumentResolver
	CSRFToken      CSRFTokenFn
	UserID         UserIDFn
	Now            func() time.Time
	Logger         *slog.Logger
}

// Handler serves the catalog admin pages.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil required dependencies are rejected at
// boot so the wire fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.ProductReader == nil {
		return nil, errors.New("web/catalog: ProductReader is required")
	}
	if deps.ProductWriter == nil {
		return nil, errors.New("web/catalog: ProductWriter is required")
	}
	if deps.ArgumentReader == nil {
		return nil, errors.New("web/catalog: ArgumentReader is required")
	}
	if deps.ArgumentWriter == nil {
		return nil, errors.New("web/catalog: ArgumentWriter is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("web/catalog: Resolver is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/catalog: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/catalog: UserID is required")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts every endpoint on mux. Go 1.22 method+pattern syntax
// gives r.PathValue resolution for the keyed routes.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog", h.list)
	mux.HandleFunc("GET /catalog/new", h.newProductForm)
	mux.HandleFunc("POST /catalog", h.createProduct)
	mux.HandleFunc("GET /catalog/{id}", h.detail)
	mux.HandleFunc("GET /catalog/{id}/edit", h.editProductForm)
	mux.HandleFunc("PATCH /catalog/{id}", h.updateProduct)
	mux.HandleFunc("DELETE /catalog/{id}", h.deleteProduct)
	mux.HandleFunc("GET /catalog/{id}/preview", h.preview)
	mux.HandleFunc("GET /catalog/{id}/arguments/new", h.newArgumentForm)
	mux.HandleFunc("POST /catalog/{id}/arguments", h.createArgument)
	mux.HandleFunc("GET /catalog/{id}/arguments/{arg_id}/edit", h.editArgumentForm)
	mux.HandleFunc("PATCH /catalog/{id}/arguments/{arg_id}", h.updateArgument)
	mux.HandleFunc("DELETE /catalog/{id}/arguments/{arg_id}", h.deleteArgument)
}

// ---------------------------------------------------------------------------
// Product handlers
// ---------------------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	products, err := h.deps.ProductReader.ListByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list products", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, http.StatusOK, pageTmpl, pageData{
		TenantName:  tenant.Name,
		GeneratedAt: h.deps.Now().UTC().Format(time.RFC3339),
		Rows:        rowsFromProducts(products),
		CSRFMeta:    csrf.MetaTag(token),
		HXHeaders:   csrf.HXHeadersAttr(token),
	})
}

func (h *Handler) newProductForm(w http.ResponseWriter, _ *http.Request) {
	h.writeHTML(w, http.StatusOK, productFormTmpl, productFormData{
		Action: "/catalog",
		Method: "post",
		IsNew:  true,
	})
}

func (h *Handler) createProduct(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseProductForm(r)
	if verr != nil {
		h.renderProductFormError(w, http.StatusUnprocessableEntity, productFormData{
			Action: "/catalog",
			Method: "post",
			IsNew:  true,
			Name:   in.Name, Description: in.Description, PriceCents: in.PriceCents, TagsRaw: in.TagsRaw,
		}, verr)
		return
	}
	p, err := catalog.NewProduct(tenant.ID, in.Name, in.Description, in.PriceCents, in.Tags, h.deps.Now().UTC())
	if err != nil {
		h.renderProductFormError(w, http.StatusUnprocessableEntity, productFormData{
			Action: "/catalog",
			Method: "post",
			IsNew:  true,
			Name:   in.Name, Description: in.Description, PriceCents: in.PriceCents, TagsRaw: in.TagsRaw,
		}, formError("name", domainProductMessage(err)))
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ProductWriter.SaveProduct(r.Context(), p, actor); err != nil {
		h.fail(w, http.StatusInternalServerError, "save product", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusCreated)
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	p, err := h.deps.ProductReader.GetByID(r.Context(), tenant.ID, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get product", err)
		return
	}
	args, err := h.deps.ArgumentReader.ListByProduct(r.Context(), tenant.ID, productID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list arguments", err)
		return
	}
	preview, src := h.resolveForPreview(r.Context(), tenant.ID, productID, "", "")
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, http.StatusOK, detailTmpl, detailData{
		Product:   rowFromProduct(p),
		Arguments: rowsFromArguments(args),
		Preview:   previewData{Argument: rowFromPreview(preview), Source: src},
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
	})
}

func (h *Handler) editProductForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	p, err := h.deps.ProductReader.GetByID(r.Context(), tenant.ID, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get product", err)
		return
	}
	h.writeHTML(w, http.StatusOK, productFormTmpl, productFormData{
		Action:      "/catalog/" + productID.String(),
		Method:      "patch",
		IsNew:       false,
		Name:        p.Name(),
		Description: p.Description(),
		PriceCents:  p.PriceCents(),
		TagsRaw:     strings.Join(p.Tags(), ", "),
	})
}

func (h *Handler) updateProduct(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	existing, err := h.deps.ProductReader.GetByID(r.Context(), tenant.ID, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get product", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseProductForm(r)
	formAction := "/catalog/" + productID.String()
	if verr != nil {
		h.renderProductFormError(w, http.StatusUnprocessableEntity, productFormData{
			Action: formAction,
			Method: "patch",
			Name:   in.Name, Description: in.Description, PriceCents: in.PriceCents, TagsRaw: in.TagsRaw,
		}, verr)
		return
	}
	// Replay the update through HydrateProduct + Rename/SetPrice so we
	// preserve created_at and bounce the same validation rules the
	// constructor enforces.
	now := h.deps.Now().UTC()
	updated := catalog.HydrateProduct(existing.ID(), existing.TenantID(),
		in.Name, in.Description, in.PriceCents, in.Tags, existing.CreatedAt(), now)
	// HydrateProduct skips invariants; re-run NewProduct against the
	// new field set to surface ErrInvalidProduct shaped errors and
	// re-clean tags. We discard the resulting product and copy the
	// validated state via a fresh hydrate so the persisted id remains
	// stable. If NewProduct rejects, surface the error in the form.
	if _, derr := catalog.NewProduct(existing.TenantID(), in.Name, in.Description, in.PriceCents, in.Tags, now); derr != nil {
		h.renderProductFormError(w, http.StatusUnprocessableEntity, productFormData{
			Action: formAction,
			Method: "patch",
			Name:   in.Name, Description: in.Description, PriceCents: in.PriceCents, TagsRaw: in.TagsRaw,
		}, formError("name", domainProductMessage(derr)))
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ProductWriter.SaveProduct(r.Context(), updated, actor); err != nil {
		h.fail(w, http.StatusInternalServerError, "save product", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusOK)
}

func (h *Handler) deleteProduct(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ProductWriter.DeleteProduct(r.Context(), tenant.ID, productID, actor); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "delete product", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusOK)
}

// ---------------------------------------------------------------------------
// Argument handlers
// ---------------------------------------------------------------------------

func (h *Handler) newArgumentForm(w http.ResponseWriter, r *http.Request) {
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	h.writeHTML(w, http.StatusOK, argumentFormTmpl, argumentFormData{
		Action:        "/catalog/" + productID.String() + "/arguments",
		Method:        "post",
		IsNew:         true,
		ProductID:     productID.String(),
		ScopeType:     string(catalog.ScopeTenant),
		AllowedScopes: allowedScopes,
	})
}

func (h *Handler) createArgument(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseArgumentForm(r)
	action := "/catalog/" + productID.String() + "/arguments"
	if verr != nil {
		h.renderArgumentFormError(w, http.StatusUnprocessableEntity, argumentFormData{
			Action:        action,
			Method:        "post",
			IsNew:         true,
			ProductID:     productID.String(),
			ScopeType:     in.ScopeType,
			ScopeID:       in.ScopeID,
			Text:          in.Text,
			AllowedScopes: allowedScopes,
		}, verr)
		return
	}
	arg, err := catalog.NewProductArgument(tenant.ID, productID,
		catalog.ScopeAnchor{Type: catalog.ScopeType(in.ScopeType), ID: in.ScopeID},
		in.Text, h.deps.Now().UTC())
	if err != nil {
		h.renderArgumentFormError(w, http.StatusUnprocessableEntity, argumentFormData{
			Action:        action,
			Method:        "post",
			IsNew:         true,
			ProductID:     productID.String(),
			ScopeType:     in.ScopeType,
			ScopeID:       in.ScopeID,
			Text:          in.Text,
			AllowedScopes: allowedScopes,
		}, formError("text", domainArgumentMessage(err)))
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ArgumentWriter.SaveArgument(r.Context(), arg, actor); err != nil {
		if errors.Is(err, catalog.ErrDuplicateArgument) {
			h.renderArgumentFormError(w, http.StatusConflict, argumentFormData{
				Action:        action,
				Method:        "post",
				IsNew:         true,
				ProductID:     productID.String(),
				ScopeType:     in.ScopeType,
				ScopeID:       in.ScopeID,
				Text:          in.Text,
				AllowedScopes: allowedScopes,
			}, formError("scope_id", "já existe um argumento para esse escopo — edite o existente"))
			return
		}
		h.fail(w, http.StatusInternalServerError, "save argument", err)
		return
	}
	h.renderDetailPartial(w, r, tenant.ID, productID, http.StatusCreated)
}

func (h *Handler) editArgumentForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	argID, ok := parseArgID(r)
	if !ok {
		http.Error(w, "invalid argument id", http.StatusBadRequest)
		return
	}
	arg, ok, err := h.findArgument(r.Context(), tenant.ID, productID, argID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "find argument", err)
		return
	}
	if !ok {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	h.writeHTML(w, http.StatusOK, argumentFormTmpl, argumentFormData{
		Action:        "/catalog/" + productID.String() + "/arguments/" + argID.String(),
		Method:        "patch",
		IsNew:         false,
		ProductID:     productID.String(),
		ArgumentID:    argID.String(),
		ScopeType:     string(arg.Anchor().Type),
		ScopeID:       arg.Anchor().ID,
		Text:          arg.Text(),
		AllowedScopes: allowedScopes,
	})
}

func (h *Handler) updateArgument(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	argID, ok := parseArgID(r)
	if !ok {
		http.Error(w, "invalid argument id", http.StatusBadRequest)
		return
	}
	existing, found, err := h.findArgument(r.Context(), tenant.ID, productID, argID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "find argument", err)
		return
	}
	if !found {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.Form.Get("argument_text"))
	action := "/catalog/" + productID.String() + "/arguments/" + argID.String()
	if text == "" {
		h.renderArgumentFormError(w, http.StatusUnprocessableEntity, argumentFormData{
			Action:        action,
			Method:        "patch",
			ProductID:     productID.String(),
			ArgumentID:    argID.String(),
			ScopeType:     string(existing.Anchor().Type),
			ScopeID:       existing.Anchor().ID,
			Text:          text,
			AllowedScopes: allowedScopes,
		}, formError("argument_text", "texto do argumento é obrigatório"))
		return
	}
	if len(text) > MaxArgumentTextLen {
		h.renderArgumentFormError(w, http.StatusUnprocessableEntity, argumentFormData{
			Action:        action,
			Method:        "patch",
			ProductID:     productID.String(),
			ArgumentID:    argID.String(),
			ScopeType:     string(existing.Anchor().Type),
			ScopeID:       existing.Anchor().ID,
			Text:          text,
			AllowedScopes: allowedScopes,
		}, formError("argument_text", fmt.Sprintf("máximo %d caracteres", MaxArgumentTextLen)))
		return
	}
	now := h.deps.Now().UTC()
	if err := existing.Rewrite(text, now); err != nil {
		h.renderArgumentFormError(w, http.StatusUnprocessableEntity, argumentFormData{
			Action:        action,
			Method:        "patch",
			ProductID:     productID.String(),
			ArgumentID:    argID.String(),
			ScopeType:     string(existing.Anchor().Type),
			ScopeID:       existing.Anchor().ID,
			Text:          text,
			AllowedScopes: allowedScopes,
		}, formError("argument_text", domainArgumentMessage(err)))
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ArgumentWriter.SaveArgument(r.Context(), existing, actor); err != nil {
		h.fail(w, http.StatusInternalServerError, "save argument", err)
		return
	}
	h.renderDetailPartial(w, r, tenant.ID, productID, http.StatusOK)
}

func (h *Handler) deleteArgument(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	argID, ok := parseArgID(r)
	if !ok {
		http.Error(w, "invalid argument id", http.StatusBadRequest)
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	if err := h.deps.ArgumentWriter.DeleteArgument(r.Context(), tenant.ID, argID, actor); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "delete argument", err)
		return
	}
	h.renderDetailPartial(w, r, tenant.ID, productID, http.StatusOK)
}

// preview handles GET /catalog/{id}/preview. The resolver runs against
// the current tenant; optional query params pin a team and/or channel.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	productID, ok := parseProductID(r)
	if !ok {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	teamID := strings.TrimSpace(r.URL.Query().Get("team_id"))
	channelID := strings.TrimSpace(r.URL.Query().Get("channel_id"))
	arg, src := h.resolveForPreview(r.Context(), tenant.ID, productID, teamID, channelID)
	h.writeHTML(w, http.StatusOK, previewTmpl, previewData{
		Argument:  rowFromPreview(arg),
		Source:    src,
		TeamID:    teamID,
		ChannelID: channelID,
		ProductID: productID.String(),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// findArgument locates an argument by id and asserts it belongs to the
// supplied product. The (tenantID, argumentID) write contract on the
// repository means ListByProduct is the canonical read path; we filter
// down to the one row here. This keeps the URL hierarchy meaningful
// (/catalog/{id}/arguments/{argID} cannot pull a row from another product).
func (h *Handler) findArgument(ctx context.Context, tenantID, productID, argID uuid.UUID) (*catalog.ProductArgument, bool, error) {
	args, err := h.deps.ArgumentReader.ListByProduct(ctx, tenantID, productID)
	if err != nil {
		return nil, false, err
	}
	for _, a := range args {
		if a != nil && a.ID() == argID {
			return a, true, nil
		}
	}
	return nil, false, nil
}

// resolveForPreview wraps the resolver in error-swallowing fallback
// logic: a resolver failure logs but never 500s the page; the preview
// widget shows a "padrão" badge in that case.
func (h *Handler) resolveForPreview(ctx context.Context, tenantID, productID uuid.UUID, teamID, channelID string) (*catalog.ProductArgument, ResolveSource) {
	scope := catalog.Scope{TeamID: teamID, ChannelID: channelID}
	args, err := h.deps.Resolver.ResolveArguments(ctx, tenantID, productID, scope)
	if err != nil {
		h.deps.Logger.Warn("web/catalog: preview resolve failed", "tenant_id", tenantID, "product_id", productID, "err", err)
		return nil, SourceNone
	}
	if len(args) == 0 {
		return nil, SourceNone
	}
	first := args[0]
	return first, sourceFromAnchorType(first.Anchor().Type)
}

func (h *Handler) renderListPartial(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, status int) {
	products, err := h.deps.ProductReader.ListByTenant(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list after mutation", err)
		return
	}
	h.writeHTML(w, status, listPartialTmpl, listPartialData{
		Rows: rowsFromProducts(products),
		Now:  h.deps.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) renderDetailPartial(w http.ResponseWriter, r *http.Request, tenantID, productID uuid.UUID, status int) {
	p, err := h.deps.ProductReader.GetByID(r.Context(), tenantID, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get product after mutation", err)
		return
	}
	args, err := h.deps.ArgumentReader.ListByProduct(r.Context(), tenantID, productID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list arguments after mutation", err)
		return
	}
	preview, src := h.resolveForPreview(r.Context(), tenantID, productID, "", "")
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, status, detailPartialTmpl, detailData{
		Product:   rowFromProduct(p),
		Arguments: rowsFromArguments(args),
		Preview:   previewData{Argument: rowFromPreview(preview), Source: src},
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
	})
}

// parseProductID pulls and validates the {id} path value.
func parseProductID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// parseArgID pulls and validates the {arg_id} path value.
func parseArgID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("arg_id")))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// productFormInput is the validated shape of the product form. TagsRaw
// preserves the operator's typing so the re-render after a validation
// failure keeps their text intact.
type productFormInput struct {
	Name        string
	Description string
	PriceCents  int
	Tags        []string
	TagsRaw     string
}

func parseProductForm(r *http.Request) (productFormInput, *FormError) {
	out := productFormInput{
		Name:        strings.TrimSpace(r.Form.Get("name")),
		Description: r.Form.Get("description"),
		TagsRaw:     r.Form.Get("tags"),
	}
	if out.Name == "" {
		return out, formError("name", "nome do produto é obrigatório")
	}
	if len(out.Name) > MaxNameLen {
		return out, formError("name", fmt.Sprintf("máximo %d caracteres", MaxNameLen))
	}
	if len(out.Description) > MaxDescriptionLen {
		return out, formError("description", fmt.Sprintf("máximo %d caracteres", MaxDescriptionLen))
	}
	priceRaw := strings.TrimSpace(r.Form.Get("price_cents"))
	if priceRaw == "" {
		priceRaw = "0"
	}
	price, err := strconv.Atoi(priceRaw)
	if err != nil {
		return out, formError("price_cents", "informe um inteiro de centavos válido")
	}
	if price < 0 {
		return out, formError("price_cents", "preço não pode ser negativo")
	}
	if price > MaxPriceCents {
		return out, formError("price_cents", fmt.Sprintf("máximo %d centavos", MaxPriceCents))
	}
	out.PriceCents = price
	tags := splitTags(out.TagsRaw)
	if len(tags) > MaxTags {
		return out, formError("tags", fmt.Sprintf("máximo %d tags", MaxTags))
	}
	for _, t := range tags {
		if len(t) > MaxTagLen {
			return out, formError("tags", fmt.Sprintf("cada tag deve ter no máximo %d caracteres", MaxTagLen))
		}
	}
	out.Tags = tags
	return out, nil
}

// splitTags accepts comma-separated tags and returns the trimmed,
// non-empty values. An empty input yields a nil slice.
func splitTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// argumentFormInput is the validated shape of the argument form.
type argumentFormInput struct {
	ScopeType string
	ScopeID   string
	Text      string
}

func parseArgumentForm(r *http.Request) (argumentFormInput, *FormError) {
	out := argumentFormInput{
		ScopeType: strings.TrimSpace(r.Form.Get("scope_type")),
		ScopeID:   strings.TrimSpace(r.Form.Get("scope_id")),
		Text:      strings.TrimSpace(r.Form.Get("argument_text")),
	}
	if !catalog.ScopeType(out.ScopeType).Valid() {
		return out, formError("scope_type", "escolha um escopo válido (tenant, team ou channel)")
	}
	if out.ScopeID == "" {
		return out, formError("scope_id", "informe o identificador do escopo")
	}
	if len(out.ScopeID) > MaxScopeIDLen {
		return out, formError("scope_id", fmt.Sprintf("máximo %d caracteres", MaxScopeIDLen))
	}
	if out.Text == "" {
		return out, formError("argument_text", "texto do argumento é obrigatório")
	}
	if len(out.Text) > MaxArgumentTextLen {
		return out, formError("argument_text", fmt.Sprintf("máximo %d caracteres", MaxArgumentTextLen))
	}
	return out, nil
}

// FormError is the typed validation error the handler returns to the
// re-render path. Field names match the form input `name` attribute so
// the template can highlight the offending control.
type FormError struct {
	Field   string
	Message string
}

func (e *FormError) Error() string { return e.Field + ": " + e.Message }

func formError(field, message string) *FormError { return &FormError{Field: field, Message: message} }

// domainProductMessage maps the catalog package errors a NewProduct call
// can return into operator-facing copy.
func domainProductMessage(err error) string {
	switch {
	case errors.Is(err, catalog.ErrZeroTenant):
		return "tenant ausente — recarregue a página"
	case errors.Is(err, catalog.ErrInvalidProduct):
		return "nome / preço / tags inválidos"
	default:
		return "não foi possível salvar o produto"
	}
}

// domainArgumentMessage maps the catalog package errors a
// NewProductArgument / Rewrite call can return into operator-facing copy.
func domainArgumentMessage(err error) string {
	switch {
	case errors.Is(err, catalog.ErrZeroTenant):
		return "tenant ausente — recarregue a página"
	case errors.Is(err, catalog.ErrInvalidScope):
		return "escopo inválido"
	case errors.Is(err, catalog.ErrInvalidArgument):
		return "texto do argumento inválido"
	default:
		return "não foi possível salvar o argumento"
	}
}

// allowedScopes is the closed enum the argument form's <select> renders.
var allowedScopes = []string{
	string(catalog.ScopeTenant),
	string(catalog.ScopeTeam),
	string(catalog.ScopeChannel),
}

func (h *Handler) writeHTML(w http.ResponseWriter, status int, t htmlTemplate, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := t.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/catalog: render", "err", err)
	}
}

func (h *Handler) renderProductFormError(w http.ResponseWriter, status int, form productFormData, ferr *FormError) {
	form.FieldError = ferr.Field
	form.ErrorMessage = ferr.Message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := productFormTmpl.Execute(w, form); err != nil {
		h.deps.Logger.Error("web/catalog: render product form error", "err", err)
	}
}

func (h *Handler) renderArgumentFormError(w http.ResponseWriter, status int, form argumentFormData, ferr *FormError) {
	form.FieldError = ferr.Field
	form.ErrorMessage = ferr.Message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := argumentFormTmpl.Execute(w, form); err != nil {
		h.deps.Logger.Error("web/catalog: render argument form error", "err", err)
	}
}

// htmlTemplate is the minimal interface every render template
// satisfies — *template.Template implements Execute(w, data).
type htmlTemplate interface {
	Execute(w io.Writer, data any) error
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/catalog: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}
