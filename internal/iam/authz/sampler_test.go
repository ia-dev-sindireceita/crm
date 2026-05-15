package authz_test

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/obs"
)

func TestDeterministicSampler_RateClampedToZeroNeverSamples(t *testing.T) {
	t.Parallel()
	s := authz.NewDeterministicSampler(-0.5)
	for i := 0; i < 100; i++ {
		ctx := obs.WithRequestID(context.Background(), randomReqID(t, i))
		if s.ShouldSampleAllow(ctx, iam.Principal{}, iam.ActionTenantContactRead) {
			t.Fatal("rate=0 sampler emitted true")
		}
	}
}

func TestDeterministicSampler_RateClampedToOneAlwaysSamples(t *testing.T) {
	t.Parallel()
	s := authz.NewDeterministicSampler(5)
	for i := 0; i < 100; i++ {
		ctx := obs.WithRequestID(context.Background(), randomReqID(t, i))
		if !s.ShouldSampleAllow(ctx, iam.Principal{}, iam.ActionTenantContactRead) {
			t.Fatal("rate=1 sampler emitted false")
		}
	}
}

func TestDeterministicSampler_SameRequestIDSameVerdict(t *testing.T) {
	t.Parallel()
	s := authz.NewDeterministicSampler(0.5)
	ctx := obs.WithRequestID(context.Background(), "req-fixed-fingerprint")
	first := s.ShouldSampleAllow(ctx, iam.Principal{}, iam.ActionTenantContactRead)
	for i := 0; i < 50; i++ {
		got := s.ShouldSampleAllow(ctx, iam.Principal{}, iam.ActionTenantContactRead)
		if got != first {
			t.Fatalf("verdict flipped on iteration %d (deterministic broken)", i)
		}
	}
}

func TestDeterministicSampler_AcrossManyRequestsConverges(t *testing.T) {
	// Sanity check that ~rate fraction of requests sample positive over
	// a large sample. We pick a wide tolerance because FNV is uniform
	// but not cryptographic — exact rate is not a contract.
	t.Parallel()
	const n = 4000
	const rate = 0.25
	s := authz.NewDeterministicSampler(rate)
	hits := 0
	for i := 0; i < n; i++ {
		ctx := obs.WithRequestID(context.Background(), randomReqID(t, i))
		if s.ShouldSampleAllow(ctx, iam.Principal{}, iam.ActionTenantContactRead) {
			hits++
		}
	}
	frac := float64(hits) / float64(n)
	// Hash distribution is uniform; with n=4000 we expect 1000 ± noise.
	// A 5%-points tolerance is generous and forgives any future rate
	// drift in the underlying hash without making the test flaky.
	if frac < rate-0.05 || frac > rate+0.05 {
		t.Fatalf("sampling fraction %v outside tolerance [%v, %v]", frac, rate-0.05, rate+0.05)
	}
}

func TestDeterministicSampler_NoRequestIDFallsBackToRandom(t *testing.T) {
	t.Parallel()
	// rate=0 / rate=1 already covered above; this case verifies the
	// no-request-id branch with a middling rate is consistent with the
	// extreme paths (does not panic, returns a bool).
	s := authz.NewDeterministicSampler(0.5)
	got := s.ShouldSampleAllow(context.Background(), iam.Principal{}, iam.ActionTenantContactRead)
	_ = got
}

func TestAlwaysAndNeverSampleHonourContract(t *testing.T) {
	t.Parallel()
	if !(authz.AlwaysSample{}).ShouldSampleAllow(context.Background(), iam.Principal{}, iam.ActionTenantContactRead) {
		t.Fatal("AlwaysSample returned false")
	}
	if (authz.NeverSample{}).ShouldSampleAllow(context.Background(), iam.Principal{}, iam.ActionTenantContactRead) {
		t.Fatal("NeverSample returned true")
	}
}

// randomReqID generates a stable-but-different request id per index.
// We avoid time/randomness so the test is deterministic.
func randomReqID(t *testing.T, i int) string {
	t.Helper()
	const alphabet = "abcdef0123456789"
	var b [16]byte
	v := uint64(i*2654435761) ^ uint64(i)
	for k := range b {
		b[k] = alphabet[v&0xF]
		v >>= 4
	}
	return string(b[:])
}
