// Package version exposes build-time identity for the CRM binaries.
//
// The commit SHA is injected at link time via:
//
//	go build -ldflags="-X github.com/pericles-luz/crm/internal/version.commitSHA=$(git rev-parse HEAD)"
//
// When the ldflag is not supplied (dev `go run`, `go test`), CommitSHA
// returns the literal string "unknown" so callers never have to special-case
// an empty value. The package has zero runtime dependencies and is safe to
// import from any layer, including domain code — it is a constant, not a
// service.
package version

// commitSHA is set at link time. The "unknown" default keeps callers
// honest: every code path that exposes the value (e.g. /health) emits a
// non-empty string even on a vanilla `go test`/`go run` invocation, which
// is what the staging smoke-gate (cd-stg.yml) and operator tooling expect.
//
// Do not initialise to "" — an empty string is indistinguishable from
// "field absent" in JSON consumers like jq, and the cd-stg gate would
// then mistake a missing ldflag for "container is still starting".
var commitSHA = "unknown"

// CommitSHA returns the build commit identifier injected at link time, or
// "unknown" when no value was provided.
func CommitSHA() string {
	if commitSHA == "" {
		return "unknown"
	}
	return commitSHA
}
