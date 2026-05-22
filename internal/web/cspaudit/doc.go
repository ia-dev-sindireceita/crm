// Package cspaudit owns the SIN-63275 / SIN-63278 cross-repo guard
// that asserts every inline <style> / <script> tag emitted by
// CSP-covered handler templates carries a `nonce=` attribute.
//
// The package has no production exports — the audit lives in
// audit_test.go and runs as part of the regular `go test ./...`
// pipeline. Putting it under its own directory keeps the audit
// independent from any one feature package and lets the test walk
// the repo from a stable cwd.
package cspaudit
