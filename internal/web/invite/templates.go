package invite

import "html/template"

// formData drives the set-password form (the valid-token happy path).
type formData struct {
	TenantName       string
	Token            string // echoed into the form action; opaque, high-entropy
	Email            string // shown read-only so the invitee knows the account
	MinLen           int
	PasswordError    string
	TenantThemeStyle template.CSS
	CSPNonce         string
}

// errorData drives the generic error page (invalid / expired / consumed).
type errorData struct {
	TenantName       string
	TenantThemeStyle template.CSS
	CSPNonce         string
}

// doneData drives the success page shown after the password is set.
type doneData struct {
	TenantName       string
	TenantThemeStyle template.CSS
	CSPNonce         string
}

var root = template.Must(template.New("invite").Parse(
	formSrc + errorSrc + doneSrc,
))

var (
	formTmpl  = root.Lookup("invite.form")
	errorTmpl = root.Lookup("invite.error")
	doneTmpl  = root.Lookup("invite.done")
)

// head is the shared <head> block: token → components stylesheets, tenant
// theme wins last, no HTMX (the page is a plain progressive-enhancement form
// — a single POST, no partial swaps needed).
const headSrc = `<meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="referrer" content="no-referrer">
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">`

// invite.form — the set-password page for a valid token. The form POSTs back
// to /invite/{{.Token}} (the token stays the capability; no hidden session).
const formSrc = `{{define "invite.form"}}<!doctype html>
<html lang="pt-BR">
<head>
  ` + headSrc + `
  <title>Definir senha — {{.TenantName}}</title>
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
</head>
<body>
  <main class="auth-shell" role="main" aria-label="Definir senha">
    <header class="auth-header">
      <h1>Definir sua senha</h1>
      <p class="auth-lede">Bem-vindo(a) a {{.TenantName}}. Crie uma senha para acessar sua conta <strong>{{.Email}}</strong>.</p>
    </header>
    {{if .PasswordError}}<div class="alert alert--danger" role="alert">{{.PasswordError}}</div>{{end}}
    <form method="post" action="/invite/{{.Token}}" class="auth-form" autocomplete="off">
      <label for="password">Nova senha</label>
      <input type="password" id="password" name="password" required minlength="{{.MinLen}}"
             autocomplete="new-password" aria-describedby="password-help">
      <p id="password-help" class="form-help">Mínimo de {{.MinLen}} caracteres. Evite senhas comuns ou iguais ao seu e-mail.</p>

      <label for="password_confirm">Confirme a senha</label>
      <input type="password" id="password_confirm" name="password_confirm" required minlength="{{.MinLen}}"
             autocomplete="new-password">

      <button type="submit" class="btn btn--primary">Definir senha</button>
    </form>
  </main>
</body>
</html>
{{end}}`

// invite.error — one generic page for invalid / expired / already-used
// tokens. No detail is leaked about WHICH condition failed (no oracle).
const errorSrc = `{{define "invite.error"}}<!doctype html>
<html lang="pt-BR">
<head>
  ` + headSrc + `
  <title>Convite indisponível — {{.TenantName}}</title>
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
</head>
<body>
  <main class="auth-shell" role="main" aria-label="Convite indisponível">
    <header class="auth-header">
      <h1>Link indisponível</h1>
      <p class="auth-lede">Este link para definir a senha é inválido, expirou ou já foi utilizado. Peça um novo convite ao administrador do seu CRM.</p>
    </header>
    <p><a class="btn btn--secondary" href="/login">Ir para o login</a></p>
  </main>
</body>
</html>
{{end}}`

// invite.done — success page after the password is set. The token is now
// spent; the invitee proceeds to /login.
const doneSrc = `{{define "invite.done"}}<!doctype html>
<html lang="pt-BR">
<head>
  ` + headSrc + `
  <title>Senha definida — {{.TenantName}}</title>
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
</head>
<body>
  <main class="auth-shell" role="main" aria-label="Senha definida">
    <header class="auth-header">
      <h1>Senha definida com sucesso</h1>
      <p class="auth-lede">Sua senha foi criada. Agora você já pode acessar o CRM de {{.TenantName}}.</p>
    </header>
    <p><a class="btn btn--primary" href="/login">Entrar</a></p>
  </main>
</body>
</html>
{{end}}`
