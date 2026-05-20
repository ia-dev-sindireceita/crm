package version

import "testing"

// These tests mutate the package-level commitSHA so they must NOT use
// t.Parallel — running concurrently would race the shared variable and
// produce flakes on `go test -race`.

func TestCommitSHA_DefaultsToUnknown(t *testing.T) {
	orig := commitSHA
	t.Cleanup(func() { commitSHA = orig })

	commitSHA = "unknown"
	if got := CommitSHA(); got != "unknown" {
		t.Fatalf("CommitSHA()=%q, want %q", got, "unknown")
	}
}

func TestCommitSHA_EmptyStringFallsBackToUnknown(t *testing.T) {
	orig := commitSHA
	t.Cleanup(func() { commitSHA = orig })

	commitSHA = ""
	if got := CommitSHA(); got != "unknown" {
		t.Fatalf("CommitSHA()=%q, want %q", got, "unknown")
	}
}

func TestCommitSHA_ReturnsInjectedValue(t *testing.T) {
	orig := commitSHA
	t.Cleanup(func() { commitSHA = orig })

	const injected = "0123456789abcdef0123456789abcdef01234567"
	commitSHA = injected
	if got := CommitSHA(); got != injected {
		t.Fatalf("CommitSHA()=%q, want %q", got, injected)
	}
}
