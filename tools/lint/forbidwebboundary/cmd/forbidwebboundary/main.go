// forbidwebboundary is a go vet tool that fails when any package under
// internal/web/... imports the inbox domain root (internal/inbox)
// directly. Wire it up in CI with:
//
//	go vet -vettool=$(which forbidwebboundary) ./internal/web/...
//
// Build with: go build -o bin/forbidwebboundary ./tools/lint/forbidwebboundary/cmd/forbidwebboundary
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/pericles-luz/crm/tools/lint/forbidwebboundary"
)

func main() {
	singlechecker.Main(forbidwebboundary.Analyzer)
}
