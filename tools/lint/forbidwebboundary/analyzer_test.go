package forbidwebboundary_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/forbidwebboundary"
)

// TestAnalyzer_FlagsForbiddenInboxImportFromWeb verifies the happy
// rejection path: a web/inbox package that imports the inbox domain
// root directly is flagged.
func TestAnalyzer_FlagsForbiddenInboxImportFromWeb(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidwebboundary.Analyzer,
		"github.com/pericles-luz/crm/internal/web/inbox/bad")
}

// TestAnalyzer_AllowsUseCaseImportFromWeb proves a web/inbox package
// that only imports the use-case path passes cleanly.
func TestAnalyzer_AllowsUseCaseImportFromWeb(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidwebboundary.Analyzer,
		"github.com/pericles-luz/crm/internal/web/inbox/good")
}

// TestAnalyzer_DoesNotFireOutsideWeb covers the scope check: a
// non-web package importing internal/inbox is not flagged.
func TestAnalyzer_DoesNotFireOutsideWeb(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidwebboundary.Analyzer,
		"github.com/pericles-luz/crm/internal/inbox/usecase/usercase")
}

// TestAnalyzer_HonorsAnnotatedOverride proves a // forbidwebboundary:ok
// <reason> comment silences the rule for one import line.
func TestAnalyzer_HonorsAnnotatedOverride(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidwebboundary.Analyzer,
		"github.com/pericles-luz/crm/internal/web/inbox/override")
}

// TestAnalyzer_RejectsBareOverride proves the bare marker (no
// justification) does NOT silence the rule.
func TestAnalyzer_RejectsBareOverride(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidwebboundary.Analyzer,
		"github.com/pericles-luz/crm/internal/web/inbox/bareoverride")
}
