package authz

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"hash/fnv"
	"math"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/obs"
)

// DeterministicSampler keeps a request's allow decisions in-sample (or
// out-of-sample) consistently by hashing its request_id. This is the
// ADR 0004 §6 baseline: production runs at 1% (Rate = 0.01).
//
// When the context has no request_id the sampler falls back to
// crypto/rand. Background goroutines and tests that never set a
// request_id therefore still produce uniform random samples; correctness
// is preserved at the cost of forensic reproducibility for those
// (vanishingly rare) call sites.
type DeterministicSampler struct {
	// Rate is the fraction in [0.0, 1.0] of allow decisions kept. The
	// AC value for production is 0.01 (1%).
	Rate float64
}

// NewDeterministicSampler returns a sampler with the given rate.
// rate < 0 is clamped to 0 (never sample); rate > 1 is clamped to 1
// (always sample). Both extremes are useful in tests.
func NewDeterministicSampler(rate float64) *DeterministicSampler {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return &DeterministicSampler{Rate: rate}
}

// ShouldSampleAllow reports whether the allow decision for the current
// request should be persisted. The principal and action are ignored at
// the baseline rate; future variants (e.g. boost rate for PII actions)
// can branch on them without changing the seam.
func (s *DeterministicSampler) ShouldSampleAllow(ctx context.Context, _ iam.Principal, _ iam.Action) bool {
	if s.Rate <= 0 {
		return false
	}
	if s.Rate >= 1 {
		return true
	}
	if rid := obs.RequestIDFromContext(ctx); rid != "" {
		return hashRate(rid) < s.Rate
	}
	return randRate() < s.Rate
}

// hashRate maps the request_id to a value in [0.0, 1.0) deterministically.
// FNV-1a 32-bit is enough — collisions are irrelevant for sampling, only
// the uniform distribution matters.
func hashRate(rid string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(rid))
	return float64(h.Sum32()) / float64(math.MaxUint32+1)
}

// randRate returns a fresh uniform draw from [0.0, 1.0). crypto/rand
// failures degrade to "out of sample" (the function returns 1.0) so a
// transient RNG outage cannot accidentally flood the audit table with
// allow rows.
func randRate() float64 {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return 1
	}
	return float64(binary.BigEndian.Uint64(b[:])) / float64(math.MaxUint64) //nolint:gosec // uniform [0,1) is the only property we need
}

// AlwaysSample is a Sampler that keeps every allow decision. Useful for
// tests that need to assert "this allow decision was recorded".
type AlwaysSample struct{}

// ShouldSampleAllow always returns true.
func (AlwaysSample) ShouldSampleAllow(context.Context, iam.Principal, iam.Action) bool {
	return true
}

// NeverSample is a Sampler that drops every allow decision. Useful for
// tests that need to assert "no allow row was written".
type NeverSample struct{}

// ShouldSampleAllow always returns false.
func (NeverSample) ShouldSampleAllow(context.Context, iam.Principal, iam.Action) bool {
	return false
}
