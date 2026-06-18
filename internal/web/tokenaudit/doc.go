// Package tokenaudit owns the SIN-65116 WCAG contrast guard that
// asserts the design-token colour pairs in web/static/css/tokens.css
// clear WCAG 2.1 AA (4.5:1 for body text) on the surfaces they are
// actually rendered on — for both the light :root palette and the
// [data-theme="dark"] rebind.
//
// The package has no production exports — the audit lives in
// contrast_test.go and runs as part of the regular `go test ./...`
// pipeline. Putting it under its own directory keeps the audit
// independent from any one feature package and lets the test walk the
// repo from a stable cwd, mirroring internal/web/cspaudit.
package tokenaudit
