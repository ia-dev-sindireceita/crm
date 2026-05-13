package csrf

import (
	"html/template"
)

// FormHidden returns the hidden <input> element to embed inside any
// <form> that issues a state-changing request. ADR 0073 D1 ships this
// as a templ helper; here it is the html/template equivalent until the
// templ migration lands. The token is HTML-escaped so a malicious value
// (e.g. a token forged with quotes) cannot break out of the attribute.
func FormHidden(token string) template.HTML {
	return template.HTML(`<input type="hidden" name="` + FormField + `" value="` + template.HTMLEscapeString(token) + `">`)
}

// MetaTag returns the <meta name="csrf-token"> tag for the
// authenticated layout's <head>. HTMX reads it via hx-headers (see
// HXHeadersAttr below).
func MetaTag(token string) template.HTML {
	return template.HTML(`<meta name="csrf-token" content="` + template.HTMLEscapeString(token) + `">`)
}

// HXHeadersAttr returns the hx-headers attribute string to attach to
// <body> in the authenticated layout. Every HTMX request emitted by the
// page picks up the X-CSRF-Token header automatically. JSEscapeString
// guards the JSON-quoted value against ' / " / </script> injection.
func HXHeadersAttr(token string) template.HTMLAttr {
	return template.HTMLAttr(`hx-headers='{"` + HeaderName + `": "` + template.JSEscapeString(token) + `"}'`)
}
