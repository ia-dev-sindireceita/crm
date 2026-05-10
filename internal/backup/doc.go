// Package backup contains regression tests for the encrypted Postgres backup
// pipeline (scripts/backup.sh, scripts/restore-drill.sh, infra/age-backup.pub).
//
// There is no production Go code in this package; the runtime tooling is
// bash. The tests verify the crypto invariants and the script wiring so a
// future change cannot silently turn encryption off, point at the wrong
// recipient, or break the pg_dump | age | aws s3 cp pipeline.
//
// Coverage policy: this package is intentionally pure-test. `go test -cover`
// reporting "[no statements]" is correct, not a gap. If any production .go
// file is later added here, that production code must land with >= 85%
// statement coverage of its own behaviour, per the company quality bar.
// Tests in this file already cover the security invariants exercised by the
// shell pipeline — keep adding tests there for shell-side changes; do not
// dilute the bar by introducing trivial production helpers.
//
// SIN-62250.
package backup
