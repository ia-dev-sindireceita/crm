package privacy

import "html/template"

// pageTmpl is the full page render — single template, no partials.
// Stays close to the funnel/contacts pattern: server-rendered,
// CSP-safe (no inline JS, no inline event handlers), HTMX optional
// (the page works fine without HTMX loaded).
//
// CSP nonce: this page renders inside the csp.Middleware envelope so
// any future <script> would need to participate in the nonce flow.
// We deliberately ship zero scripts here.
//
// The Subprocessor table is the LGPD-load-bearing element. Each row
// has the four DPA §4 fields (name, kind, purpose, data, policy
// link). The first row is the OpenRouter row — placement is
// intentional so a quick visual scan answers the decisão #8
// question "who is the LLM sub-processor?".
//
// .ActiveModel is rendered prominently above the table so a tenant
// who wants to know "which model is sending my data" sees it on the
// first viewport.
var pageTmpl = template.Must(template.New("privacy.page").Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Privacidade e sub-processadores — {{.TenantName}}</title>
  <link rel="stylesheet" href="/static/css/privacy.css">
</head>
<body>
  <main class="privacy-shell" role="main" aria-label="Privacidade">
    <header class="privacy-header">
      <h1>Privacidade e sub-processadores</h1>
      <p class="privacy-lede">
        Esta página lista as empresas terceiras que tratam dados pessoais
        em nome de <strong>{{.TenantName}}</strong> para operar o CRM
        Sindireceita. As cláusulas integrais estão no
        <a href="/settings/privacy/dpa.md" rel="external"
           download="{{.DPAFilename}}">Acordo de Processamento de Dados
        (DPA), versão {{.DPAVersion}}</a>.
      </p>
    </header>

    <section class="privacy-model" aria-labelledby="model-title">
      <h2 id="model-title">Modelo de IA ativo</h2>
      <p>
        Modelo OpenRouter atualmente resolvido para este tenant:
        <code class="privacy-model__value">{{.ActiveModel}}</code>
      </p>
      <p class="privacy-model__note">
        A resolução segue a cascata configurada em
        <em>ai-policy</em>: canal &gt; equipe &gt; tenant. Mudanças
        feitas na política de IA passam a valer na próxima chamada.
      </p>
    </section>

    <section class="privacy-subprocessors" aria-labelledby="subprocessors-title">
      <h2 id="subprocessors-title">Sub-processadores de dados</h2>
      <table class="privacy-table">
        <thead>
          <tr>
            <th scope="col">Sub-processador</th>
            <th scope="col">Categoria</th>
            <th scope="col">Finalidade</th>
            <th scope="col">Dados tratados</th>
            <th scope="col">Política</th>
          </tr>
        </thead>
        <tbody>
        {{- range .Subprocessors}}
          <tr class="privacy-row privacy-row--{{.Status}}">
            <td>{{.Name}}</td>
            <td>{{.Kind}}</td>
            <td>{{.Purpose}}</td>
            <td>{{.DataHandled}}</td>
            <td>
              {{- if .PolicyURL -}}
                <a href="{{.PolicyURL}}" rel="external noopener noreferrer" target="_blank">Ver política</a>
              {{- else -}}
                <span class="privacy-pending" aria-label="A definir">—</span>
              {{- end -}}
            </td>
          </tr>
        {{- end}}
        </tbody>
      </table>
    </section>

    <section class="privacy-rights" aria-labelledby="rights-title">
      <h2 id="rights-title">Direitos do titular (LGPD)</h2>
      <p>
        Para exercer direitos previstos no art. 18 da LGPD (acesso,
        correção, eliminação, portabilidade) entre em contato pelo canal
        de privacidade do seu atendente <em>master</em>. O sistema
        executa pedidos de eliminação em até 30 dias após validação,
        conforme §3 do DPA.
      </p>
    </section>

    <footer class="privacy-footer">
      <p>
        DPA versão <strong>{{.DPAVersion}}</strong> — página gerada em
        <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.
      </p>
    </footer>
  </main>
</body>
</html>
`))
