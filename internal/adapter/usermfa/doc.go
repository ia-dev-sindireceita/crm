// Package usermfa hosts the tenant-scope adapters that wire the
// internal/iam/mfa domain layer to the tenant audit ledger and the
// no-op tenant alerter (SIN-63184, Fase 6 PR1).
//
// The mfa.Service in iam/mfa is scope-agnostic: it takes a SeedRepository,
// RecoveryStore, AuditLogger, Alerter, CodeHasher, and SeedCipher. The
// master flow wires those to master_mfa / master_recovery_code + the
// master_ops audit trigger + a Slack #alerts alerter. The tenant flow
// wires the same ports to user_mfa / user_recovery_code (the postgres
// adapters in internal/adapter/db/postgres) + the audit_log_security
// ledger (this package's TenantAuditLogger) + an audit-only NoopAlerter
// (Slack #alerts is master-only by policy).
package usermfa
