package shell_test

import "testing"

// TestRender_ImpersonationCountdownScriptIsLinked pins SIN-65379 /
// UX-F10. The impersonation banner renders a
// `<span data-impersonation-countdown>` whose live value is ticked by
// /static/js/impersonation-countdown.js. The shell layout must load
// that script (defer, external, CSP-safe from 'self') or the master
// operator sees a blank countdown pill — the original F10 defect. A
// future cascade refactor that drops the <script> tag here would
// silently regress the countdown without breaking any other test, so
// pin the exact tag.
func TestRender_ImpersonationCountdownScriptIsLinked(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{})

	mustContain(t, body, `<script src="/static/js/impersonation-countdown.js" defer></script>`)
}
