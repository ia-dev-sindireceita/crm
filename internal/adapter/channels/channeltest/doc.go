// Package channeltest holds the Postgres-real integration harness reused
// by the per-channel adapter integration suites (whatsapp / instagram /
// messenger / webchat). It is gated behind the `integration` build tag
// so the fast `go test ./...` path never tries to spin up Postgres.
//
// The harness mirrors internal/webhook/integration/harness_test.go: it
// connects to the DSN supplied via TEST_POSTGRES_DSN when set, or starts
// a postgres:16-alpine testcontainer otherwise. Migrations are applied
// once per test process from the repo's migrations/ directory.
//
// Tests that need fresh row state between cases call Truncate; the
// harness intentionally never recreates the database — applying every
// up.sql in lexical order is the schema under test, not a per-test fixture.
//
// SIN-62846: F2-09.3 / F2-10.3 — gives the whatsapp + instagram channel
// adapters a uniform E2E layer on top of the unit fakes, closing the
// "real Postgres" gap left when SIN-62795 / SIN-62796 shipped with
// adapter-fake-only coverage.
package channeltest
