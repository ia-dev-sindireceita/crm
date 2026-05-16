package aipolicyaudit

import "html/template"

// pageTmpl is the single full-page render — no partials, no JS
// framework, CSP-friendly. The /admin/audit slice uses the same
// template and just flips IsMaster so the same markup serves both
// tenant and master consoles (less template drift).
//
// HTMX is loaded on the page so the operator can refine filters
// without a hard reload (hx-get on the filter form, hx-target on the
// table tbody). The fall-back behaviour (no JS) is a normal GET
// that re-renders the full page, so the audit table works without
// HTMX too.
//
// The row tinting `audit-row--master` is the load-bearing visual
// element from decisão #10/#16: changes made under master
// impersonation appear in a red-tinted row so tenant admins can
// spot them at a glance.
var pageTmpl = template.Must(template.New("aipolicyaudit.page").Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>{{.Title}}</title>
  <link rel="stylesheet" href="/static/css/aipolicyaudit.css">
</head>
<body>
  <main class="audit-shell" role="main" aria-label="Auditoria de AI policy">
    <header class="audit-header">
      <h1>{{if .IsMaster}}Auditoria master — IA{{else}}Auditoria de AI policy{{end}}</h1>
      <p class="audit-lede">
        {{if .IsMaster -}}
          Listagem cross-tenant das mudanças em <code>ai_policy</code>.
          Tenant em foco: <code>{{.TenantName}}</code>.
        {{- else -}}
          Toda mudança em <strong>{{.TenantName}}</strong> em
          configurações de IA fica registrada aqui (campo, valor anterior,
          valor novo, autor, e marcação de sessão master quando aplicável).
          Retenção padrão: 12 meses (decisão #3, configurável por contrato).
        {{- end}}
      </p>
      <p class="audit-meta"><small>Gerado em {{.GeneratedAt}}.</small></p>
    </header>

    <section class="audit-filters" aria-labelledby="filters-title">
      <h2 id="filters-title" class="visually-hidden">Filtros</h2>
      <form method="get" action="{{.BaseURL}}" class="audit-filters__form">
        {{if .IsMaster -}}
          <input type="hidden" name="tenant" value="{{.TenantID}}">
          <input type="hidden" name="module" value="{{.ModuleParam}}">
        {{- end}}
        <label>
          Escopo:
          <select name="scope_type">
            <option value="" {{if eq .Filters.RawScopeType ""}}selected{{end}}>Todos</option>
            <option value="tenant" {{if eq .Filters.RawScopeType "tenant"}}selected{{end}}>Tenant</option>
            <option value="team" {{if eq .Filters.RawScopeType "team"}}selected{{end}}>Equipe</option>
            <option value="channel" {{if eq .Filters.RawScopeType "channel"}}selected{{end}}>Canal</option>
          </select>
        </label>
        <label>
          ID do escopo:
          <input type="text" name="scope_id" value="{{.Filters.RawScopeID}}" placeholder="ex. whatsapp">
        </label>
        <label>
          Desde:
          <input type="date" name="since" value="{{.Filters.RawSince}}">
        </label>
        <label>
          Até:
          <input type="date" name="until" value="{{.Filters.RawUntil}}">
        </label>
        <button type="submit">Filtrar</button>
      </form>
    </section>

    <section class="audit-list" aria-labelledby="list-title">
      <h2 id="list-title" class="visually-hidden">Mudanças registradas</h2>
      {{- if .Events}}
      <table class="audit-table">
        <thead>
          <tr>
            <th scope="col">Quando (UTC)</th>
            <th scope="col">Escopo</th>
            <th scope="col">Campo</th>
            <th scope="col">Valor anterior</th>
            <th scope="col">Valor novo</th>
            <th scope="col">Autor</th>
          </tr>
        </thead>
        <tbody>
        {{- range .Events}}
          <tr class="audit-row{{if .ActorMaster}} audit-row--master{{end}}">
            <td><time datetime="{{.When}}">{{.When}}</time></td>
            <td>{{.ScopeType}} · <code>{{.ScopeID}}</code></td>
            <td><code>{{.Field}}</code></td>
            <td><code>{{.Old}}</code></td>
            <td><code>{{.New}}</code></td>
            <td>
              <code>{{.ActorUserID}}</code>
              {{- if .ActorMaster}}
              <span class="audit-pill audit-pill--master" title="Mudança realizada em sessão master de impersonação">master</span>
              {{- end}}
            </td>
          </tr>
        {{- end}}
        </tbody>
      </table>
      {{- else}}
      <p class="audit-empty">Nenhuma mudança registrada para os filtros atuais.</p>
      {{- end}}
    </section>

    {{- if .NextCursor}}
    <nav class="audit-pager">
      <a class="audit-pager__next" rel="next" href="{{.BaseURL}}?{{if .IsMaster}}tenant={{.TenantID}}&module={{.ModuleParam}}&{{end}}cursor={{.NextCursor}}{{if .Filters.RawScopeType}}&scope_type={{.Filters.RawScopeType}}{{end}}{{if .Filters.RawScopeID}}&scope_id={{.Filters.RawScopeID}}{{end}}{{if .Filters.RawSince}}&since={{.Filters.RawSince}}{{end}}{{if .Filters.RawUntil}}&until={{.Filters.RawUntil}}{{end}}">Próxima página →</a>
    </nav>
    {{- end}}
  </main>
</body>
</html>`))
