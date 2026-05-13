// Package mastermfa is the HTTP adapter for the master MFA flow
// (ADR 0074). It exposes the handlers behind /m/2fa/* and the
// RequireMasterMFA middleware that gates every other master.* route.
//
// This PR ships the enrol handler only:
//
//	POST /m/2fa/enroll  → Service.Enroll → render once-only QR +
//	                      10 recovery codes + confirmation form
//
// The verify handler, the RequireMasterMFA middleware, and the
// recovery flow land in subsequent PRs.
//
// The handlers depend on a MasterContext port that exposes the
// authenticated master's UUID + email. Master session storage does
// not yet exist in this repo (sessions(0006) is tenant-only); the
// production wire-up will land alongside the master auth PR. Tests
// here inject the master directly via WithMaster.
package mastermfa
