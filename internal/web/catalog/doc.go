// Package catalog is the HTMX admin UI for the tenant product catalog
// (SIN-62907 / Fase 3 W4C, child of SIN-62196). It mounts under /catalog
// and serves the Product CRUD plus the per-scope ProductArgument editor
// that feeds the cascade resolver in internal/catalog (SIN-62902 / W2B).
//
// Routes:
//
//   - GET    /catalog
//     Full page: lists every Product for the current tenant ordered by
//     created_at ASC.
//
//   - GET    /catalog/new
//     HTMX partial form for creating a new Product.
//
//   - GET    /catalog/{id}
//     Product detail page with the per-scope ProductArgument table and
//     the cascade-preview widget.
//
//   - GET    /catalog/{id}/edit
//     HTMX partial form pre-populated with the existing Product row.
//
//   - POST   /catalog
//     Creates a Product from form fields (name, description, price_cents,
//     tags). The handler validates against catalog.NewProduct and re-renders
//     the new-form partial with a field-level error message on failure.
//
//   - PATCH  /catalog/{id}
//     Renames the Product and updates description / price / tags. Returns
//     the refreshed list partial so HTMX can swap the table out.
//
//   - DELETE /catalog/{id}
//     Hard-deletes the Product. Migration 0098 declared product_argument
//     FK ON DELETE CASCADE so arguments disappear with the Product. The
//     parent issue description requested "soft delete" but the W2B schema
//     does not carry a deleted_at column and no W4D suggestion path exists
//     yet that would consume historical references, so the AC-described
//     soft delete is rendered here as a hard delete with the cascade the
//     migration documents.
//
//   - GET    /catalog/{id}/arguments/new
//     HTMX partial form for creating a new ProductArgument.
//
//   - GET    /catalog/{id}/arguments/{arg_id}/edit
//     HTMX partial form pre-populated with the existing ProductArgument
//     row.
//
//   - POST   /catalog/{id}/arguments
//     Creates a ProductArgument (scope_type, scope_id, argument_text).
//
//   - PATCH  /catalog/{id}/arguments/{arg_id}
//     Rewrites a ProductArgument's text.
//
//   - DELETE /catalog/{id}/arguments/{arg_id}
//     Removes a ProductArgument.
//
//   - GET    /catalog/{id}/preview
//     Cascade-preview partial: given optional ?team_id= and ?channel_id=
//     query params runs the W2B resolver and shows which argument applies
//     (the most-specific match comes first; the resolver returns the
//     stable channel > team > tenant ordering).
//
// Security envelope (composed by the router):
//
//   - middleware.TenantScope resolves the tenant from the host.
//   - middleware.Auth + RequireAuth attach iam.Principal.
//   - CSRF middleware short-circuits on GET/HEAD/OPTIONS; POST,
//     PATCH, DELETE consult the session token.
//   - middleware.RequireAction(iam.ActionTenantCatalogManage) gates
//     every method (the gerente who can mutate the catalog is the
//     only role that needs to see it).
//   - csp.Middleware emits the per-request nonce; the templates here
//     ship zero <script>/<style> blocks so the page complies with the
//     strict CSP envelope from SIN-62237.
//
// SafeText: every dynamic value (Product.Name, Description, tags,
// ProductArgument.Text) reaches the templates via html/template which
// auto-escapes in HTML / attribute / URL contexts. No template uses
// template.HTML or template.JS to bypass escaping.
package catalog
