// Package privacy is the public LGPD-disclosure surface for end
// customers — SIN-63191 / Fase 6 PR4 AC #3.
//
// Mounted as GET /privacy in the tenanted (but unauthenticated) group
// so middleware.TenantScope resolves the tenant from the request host
// before the handler runs. Renders three things in one round-trip:
//
//   - the tenant's currently-published privacy policy markdown (or the
//     platform fallback when the master operator has not yet published
//     one) — sanitised through goldmark + an HTML escape pass,
//   - the policy version + last-updated stamp from
//     tenants.privacy_policy_version / privacy_policy_updated_at,
//   - the DPO contact (dpo_name + dpo_email).
//
// The page is intentionally unauthenticated: LGPD art. 9 obliges the
// controller to publish the policy in a form accessible to any data
// subject; gating it behind login defeats that obligation. The handler
// therefore composes only the safe bits — no CSRF token surface, no
// user-specific data — and writes Cache-Control: public so the policy
// can be edge-cached.
package privacy
