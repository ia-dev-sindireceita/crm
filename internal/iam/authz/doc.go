// Package authz wraps an iam.Authorizer with observability that is
// load-bearing for the F10 security finding (SIN-62254 / ADR 0004 §6):
// every deny gets a row in audit_log_security at 100% sampling, and
// allow decisions get a sampled row (1% baseline via deterministic hash
// over request_id, so the same request stays in or out of the sample
// across retries and forensic replay).
//
// The package depends on iam (for the Authorizer/Decision contract) and
// on iam/audit (for the SplitLogger port). It does NOT depend on
// net/http or chi: the Authorizer seam is HTTP-unaware. The decorator
// pattern (AuditingAuthorizer wraps an inner iam.Authorizer) keeps
// existing call sites — notably the RequireAction middleware — entirely
// unmodified; production wireup constructs the wrapped instance and
// passes it where a bare *RBACAuthorizer used to go.
//
// Decision invariance: the Decision returned by an AuditingAuthorizer
// is byte-identical to what its inner authorizer returned. Audit
// failures are logged but never alter the security verdict — the
// audit trail is best-effort observability, not a gate. Non-repudiation
// on the deny path is anchored by the request already failing closed
// (the user got 403), not by a database commit.
package authz
