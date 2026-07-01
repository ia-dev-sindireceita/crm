package audit

// SIN-66410 (follow-up from the SIN-66405 A09 review of PR #447,
// residual risk R1): guard against Go↔migration audit-vocabulary drift.
//
// The audit_log_security controlled vocabulary lives in two places that
// MUST stay in lockstep:
//
//   1. allSecurityEvents in split.go (the SecurityEvent* constants), and
//   2. the audit_log_security_event_type_check CHECK constraint in the
//      highest-numbered CHECK-extending migration (latest: 0129).
//
// Nothing else fails if a constant is added to the Go side but not to the
// CHECK: it passes IsKnown() and every unit test, then the INSERT hits the
// CHECK at runtime. WriteSecurity is best-effort (warn-logged, never
// propagated), so that failure is swallowed silently — the privilege event
// simply never lands in the ledger. That is an audit blind spot by
// construction (OWASP A09, Security Logging & Monitoring Failure).
//
// This test extracts the string literals inside the CHECK's
// `event_type IN (...)` clause and asserts that set equals
// {string(e) for e in allSecurityEvents}. It fails the build the moment the
// two drift in EITHER direction (missing from CHECK → runtime-rejected write;
// missing from Go → a live value the writers can never emit but the schema
// still accepts).
//
// White-box (package audit, not audit_test) so it can enumerate the
// unexported allSecurityEvents map without widening the public API.
//
// os.ReadFile, not go:embed: the migrations/ directory sits at the repo
// root, three levels above this package, and go:embed patterns cannot
// contain "..". `go test` runs with the package directory as the working
// directory, so the relative path below is stable.
//
// MAINTENANCE: when a future migration extends the CHECK, point
// latestCheckMigration at that new *.up.sql file. The pointer comment on
// allSecurityEvents in split.go references this test.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// latestCheckMigration is the highest-numbered migration that (re)states the
// full audit_log_security_event_type_check vocabulary. Each CHECK-extending
// migration restates the entire union (named CHECK constraints are immutable;
// DROP + ADD is the only path), so the highest-numbered one is the
// authoritative final state — reading it alone is sufficient.
const latestCheckMigration = "../../../migrations/0129_audit_log_security_channel_access.up.sql"

// checkInListRe captures the parenthesised body of `event_type IN ( ... )`.
// (?s) lets "." span newlines; the body is non-greedy up to the first
// closing paren, which is correct because the IN list contains no nested
// parentheses.
var checkInListRe = regexp.MustCompile(`(?s)event_type\s+IN\s*\((.*?)\)`)

// sqlLiteralRe captures each single-quoted SQL string literal.
var sqlLiteralRe = regexp.MustCompile(`'([^']*)'`)

// checkVocabulary parses the event_type literals out of the CHECK clause in
// the given migration file. It returns an error (not a partial result) if the
// file is missing or the CHECK shape is unrecognisable, so a moved/renamed
// migration surfaces as a loud test failure rather than a silently empty set.
func checkVocabulary(path string) (map[string]struct{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body := checkInListRe.FindSubmatch(raw)
	if body == nil {
		return nil, &parseError{path, "no `event_type IN (...)` clause found"}
	}
	lits := sqlLiteralRe.FindAllStringSubmatch(string(body[1]), -1)
	if len(lits) == 0 {
		return nil, &parseError{path, "CHECK clause contained zero string literals"}
	}
	set := make(map[string]struct{}, len(lits))
	for _, m := range lits {
		set[m[1]] = struct{}{}
	}
	return set, nil
}

type parseError struct {
	path string
	msg  string
}

func (e *parseError) Error() string { return e.path + ": " + e.msg }

func TestSecurityVocabulary_MatchesLatestCheckMigration(t *testing.T) {
	t.Parallel()

	checkSet, err := checkVocabulary(latestCheckMigration)
	if err != nil {
		t.Fatalf("could not read the CHECK vocabulary: %v\n"+
			"If a new CHECK-extending migration landed, update latestCheckMigration.", err)
	}

	goSet := make(map[string]struct{}, len(allSecurityEvents))
	for e := range allSecurityEvents {
		goSet[string(e)] = struct{}{}
	}

	var missingFromCheck []string // in Go, absent from the DB CHECK → INSERT silently rejected at runtime
	for v := range goSet {
		if _, ok := checkSet[v]; !ok {
			missingFromCheck = append(missingFromCheck, v)
		}
	}
	var missingFromGo []string // in the DB CHECK, absent from Go → schema accepts a value no writer can emit
	for v := range checkSet {
		if _, ok := goSet[v]; !ok {
			missingFromGo = append(missingFromGo, v)
		}
	}
	sort.Strings(missingFromCheck)
	sort.Strings(missingFromGo)

	if len(missingFromCheck) > 0 {
		t.Errorf("SecurityEvent constants missing from the migration CHECK (%s):\n  %s\n"+
			"These would pass IsKnown() and unit tests but be REJECTED by the CHECK at INSERT time; "+
			"because WriteSecurity is best-effort the audit row is dropped silently (OWASP A09). "+
			"Add each literal to the CHECK in a new migration.",
			latestCheckMigration, strings.Join(missingFromCheck, ", "))
	}
	if len(missingFromGo) > 0 {
		t.Errorf("event_type literals in the migration CHECK (%s) with no matching SecurityEvent constant:\n  %s\n"+
			"The schema accepts a value no writer can produce. Add the constant to split.go (and allSecurityEvents) "+
			"or remove it from the CHECK.",
			latestCheckMigration, strings.Join(missingFromGo, ", "))
	}
}
