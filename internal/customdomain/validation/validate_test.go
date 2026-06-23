package validation_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// fakeResolver is a deterministic in-memory dnsresolver.Resolver. It is
// keyed on the exact host (lowercased, since Validate lowercases before
// asking) so every test arms exactly the responses it needs.
type fakeResolver struct {
	ipAnswers map[string][]dnsresolver.IPAnswer
	ipErrs    map[string]error
	txtAns    map[string][]string
	txtErrs   map[string]error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		ipAnswers: map[string][]dnsresolver.IPAnswer{},
		ipErrs:    map[string]error{},
		txtAns:    map[string][]string{},
		txtErrs:   map[string]error{},
	}
}

func (f *fakeResolver) LookupIP(_ context.Context, host string) ([]dnsresolver.IPAnswer, error) {
	if e, ok := f.ipErrs[host]; ok {
		return nil, e
	}
	return f.ipAnswers[host], nil
}

func (f *fakeResolver) LookupTXT(_ context.Context, host string) ([]string, error) {
	if e, ok := f.txtErrs[host]; ok {
		return nil, e
	}
	return f.txtAns[host], nil
}

// recordingAuditor captures every AuditEvent so tests can assert exactly
// one event of the expected type was recorded per Validate call.
type recordingAuditor struct {
	mu     sync.Mutex
	events []validation.AuditEvent
}

func (r *recordingAuditor) Record(_ context.Context, e validation.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingAuditor) only() validation.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 1 {
		panic("expected exactly one audit event")
	}
	return r.events[0]
}

// fixedClock returns a stable time so VerifiedAt is comparable.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// helper to build a Validator with deterministic auditor + clock.
func newValidator(r dnsresolver.Resolver, a validation.Auditor) (*validation.Validator, time.Time) {
	t := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	return validation.New(r, a, fixedClock{t: t}), t
}

func TestValidate_HappyPath_PinsIPAndDNSSEC(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{
		{IP: ip("203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	r.txtAns["_crm-verify.acme.example"] = []string{"crm-verify=token-abc"}
	a := &recordingAuditor{}
	v, fixedT := newValidator(r, a)

	res, err := v.Validate(context.Background(), "acme.example", "crm-verify=token-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IP != ip("203.0.113.10") {
		t.Fatalf("pinned IP mismatch: got %v", res.IP)
	}
	if !res.VerifiedWithDNSSEC {
		t.Fatalf("DNSSEC flag should be true when every answer is signed")
	}
	if !res.VerifiedAt.Equal(fixedT) {
		t.Fatalf("VerifiedAt should equal clock.Now")
	}
	got := a.only()
	if got.Event != validation.EventValidatedOK {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventValidatedOK)
	}
	if got.Detail["ip"] != "203.0.113.10" {
		t.Fatalf("audit detail.ip = %s", got.Detail["ip"])
	}
	if got.Detail["dnssec"] != "true" {
		t.Fatalf("audit detail.dnssec = %s", got.Detail["dnssec"])
	}
}

func TestValidate_HappyPath_DNSSECFalseWhenAnyAnswerUnsigned(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{
		{IP: ip("203.0.113.10"), VerifiedWithDNSSEC: true},
		{IP: ip("203.0.113.11"), VerifiedWithDNSSEC: false},
	}
	r.txtAns["_crm-verify.acme.example"] = []string{"crm-verify=token"}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	res, err := v.Validate(context.Background(), "acme.example", "crm-verify=token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.VerifiedWithDNSSEC {
		t.Fatalf("worst-case rule: any unsigned answer must drop DNSSEC flag")
	}
}

func TestValidate_BlockedSSRF_Loopback(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["evil.example"] = []dnsresolver.IPAnswer{{IP: ip("127.0.0.1")}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "evil.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	if got := a.only(); got.Event != validation.EventBlockedSSRF {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventBlockedSSRF)
	}
	// We MUST NOT mirror the resolved IP back into the audit log.
	for _, v := range a.events[0].Detail {
		if strings.Contains(v, "127.0.0.1") {
			t.Fatalf("audit detail leaks resolved IP: %v", a.events[0].Detail)
		}
	}
}

func TestValidate_BlockedSSRF_IMDS(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["aws-meta.example"] = []dnsresolver.IPAnswer{{IP: ip("169.254.169.254")}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "aws-meta.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	if got := a.only(); got.Event != validation.EventBlockedSSRF {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventBlockedSSRF)
	}
}

func TestValidate_BlockedSSRF_PrivateIPv6(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["v6.example"] = []dnsresolver.IPAnswer{{IP: ip("fc00::1")}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "v6.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP for IPv6 ULA", err)
	}
}

func TestValidate_BlockedSSRF_MixedAnswerStillRejects(t *testing.T) {
	// One public + one private = SSRF rebinding setup; reject.
	r := newFakeResolver()
	r.ipAnswers["mixed.example"] = []dnsresolver.IPAnswer{
		{IP: ip("203.0.113.42")},
		{IP: ip("10.0.0.5")},
	}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "mixed.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("mixed-answer rebinding setup must be rejected; got %v", err)
	}
}

func TestValidate_BlockedSSRF_V4MappedV6(t *testing.T) {
	// Attacker tries to smuggle 127.0.0.1 as ::ffff:127.0.0.1.
	r := newFakeResolver()
	r.ipAnswers["smuggle.example"] = []dnsresolver.IPAnswer{{IP: ip("::ffff:127.0.0.1")}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "smuggle.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("v4-mapped-v6 loopback must be rejected; got %v", err)
	}
}

func TestValidate_BlockedSSRF_InvalidAddress(t *testing.T) {
	// An adapter that returns a zero netip.Addr is buggy, but we must
	// still refuse to validate (defence in depth).
	r := newFakeResolver()
	r.ipAnswers["zero.example"] = []dnsresolver.IPAnswer{{IP: netip.Addr{}}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "zero.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("zero IP must be treated as blocked; got %v", err)
	}
}

func TestValidate_TokenMismatch(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"crm-verify=other-token"}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "acme.example", "crm-verify=token-abc")
	if !errors.Is(err, validation.ErrTokenMismatch) {
		t.Fatalf("err = %v, want ErrTokenMismatch", err)
	}
	if got := a.only(); got.Event != validation.EventTokenMismatch {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventTokenMismatch)
	}
}

func TestValidate_TokenMatch_FromOneOfManyTXTs(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{
		"v=spf1 include:_spf.example -all",
		"crm-verify=token-abc",
	}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	if _, err := v.Validate(context.Background(), "acme.example", "crm-verify=token-abc"); err != nil {
		t.Fatalf("expected success when token is among multiple TXTs: %v", err)
	}
}

func TestValidate_NoAddress(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["empty.example"] = []dnsresolver.IPAnswer{}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "empty.example", "tok")
	if !errors.Is(err, validation.ErrNoAddress) {
		t.Fatalf("err = %v, want ErrNoAddress", err)
	}
	if got := a.only(); got.Event != validation.EventNoAddress {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventNoAddress)
	}
}

func TestValidate_ResolverError_OnIPLookup(t *testing.T) {
	r := newFakeResolver()
	r.ipErrs["bork.example"] = dnsresolver.ErrTimeout
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "bork.example", "tok")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("error chain must wrap dnsresolver.ErrTimeout; got %v", err)
	}
	if got := a.only(); got.Event != validation.EventResolverError {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventResolverError)
	}
	if got := a.events[0]; got.Detail["phase"] != "ip" {
		t.Fatalf("phase detail = %s, want ip", got.Detail["phase"])
	}
}

func TestValidate_ResolverError_OnTXTLookup(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtErrs["_crm-verify.acme.example"] = errors.New("upstream blew up")
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	_, err := v.Validate(context.Background(), "acme.example", "tok")
	if err == nil || !strings.Contains(err.Error(), "upstream blew up") {
		t.Fatalf("err must wrap TXT-lookup failure; got %v", err)
	}
	if got := a.only(); got.Event != validation.EventResolverError {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventResolverError)
	}
	if got := a.events[0]; got.Detail["phase"] != "txt" {
		t.Fatalf("phase detail = %s, want txt", got.Detail["phase"])
	}
}

func TestValidate_EmptyHost(t *testing.T) {
	a := &recordingAuditor{}
	v, _ := newValidator(newFakeResolver(), a)

	_, err := v.Validate(context.Background(), "  ", "tok")
	if !errors.Is(err, validation.ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
	if got := a.only(); got.Event != validation.EventEmptyInput || got.Detail["reason"] != "host" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidate_EmptyToken(t *testing.T) {
	a := &recordingAuditor{}
	v, _ := newValidator(newFakeResolver(), a)

	_, err := v.Validate(context.Background(), "acme.example", " ")
	if !errors.Is(err, validation.ErrEmptyToken) {
		t.Fatalf("err = %v, want ErrEmptyToken", err)
	}
	if got := a.only(); got.Event != validation.EventEmptyInput || got.Detail["reason"] != "token" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidate_HostIsLowercased(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	if _, err := v.Validate(context.Background(), "  ACME.Example ", "tok"); err != nil {
		t.Fatalf("Validate must lowercase + trim host: %v", err)
	}
}

func TestValidate_NilAuditorIsTolerated(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	v := validation.New(r, nil, fixedClock{t: time.Now()})

	if _, err := v.Validate(context.Background(), "acme.example", "tok"); err != nil {
		t.Fatalf("nil auditor must use noop fallback: %v", err)
	}
}

func TestSystemClock_NowIsUTC(t *testing.T) {
	if got := (validation.SystemClock{}).Now(); got.Location() != time.UTC {
		t.Fatalf("SystemClock.Now must be UTC, got %v", got.Location())
	}
}

func TestValidateHostOnly_HappyPath(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{
		{IP: ip("203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	if err := v.ValidateHostOnly(context.Background(), "acme.example"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := a.only()
	if got.Event != validation.EventValidatedOK {
		t.Fatalf("audit event = %s, want %s", got.Event, validation.EventValidatedOK)
	}
	if got.Detail["phase"] != "host_only" {
		t.Fatalf("phase detail = %s, want host_only", got.Detail["phase"])
	}
}

func TestValidateHostOnly_DoesNotQueryTXT(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	// Arm a TXT error: ValidateHostOnly must NOT consult TXT, so this
	// should never surface.
	r.txtErrs["_crm-verify.acme.example"] = errors.New("must not be called")
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	if err := v.ValidateHostOnly(context.Background(), "acme.example"); err != nil {
		t.Fatalf("ValidateHostOnly must not call TXT lookup; got %v", err)
	}
}

func TestValidateHostOnly_BlockedSSRF(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["evil.example"] = []dnsresolver.IPAnswer{{IP: ip("127.0.0.1")}}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	err := v.ValidateHostOnly(context.Background(), "evil.example")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	if got := a.only(); got.Event != validation.EventBlockedSSRF || got.Detail["phase"] != "host_only" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidateHostOnly_NoAddress(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["empty.example"] = []dnsresolver.IPAnswer{}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	err := v.ValidateHostOnly(context.Background(), "empty.example")
	if !errors.Is(err, validation.ErrNoAddress) {
		t.Fatalf("err = %v, want ErrNoAddress", err)
	}
	if got := a.only(); got.Event != validation.EventNoAddress || got.Detail["phase"] != "host_only" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidateHostOnly_ResolverError(t *testing.T) {
	r := newFakeResolver()
	r.ipErrs["bork.example"] = dnsresolver.ErrTimeout
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	err := v.ValidateHostOnly(context.Background(), "bork.example")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err must wrap ErrTimeout; got %v", err)
	}
	if got := a.only(); got.Event != validation.EventResolverError || got.Detail["phase"] != "host_only" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidateHostOnly_EmptyHost(t *testing.T) {
	a := &recordingAuditor{}
	v, _ := newValidator(newFakeResolver(), a)

	err := v.ValidateHostOnly(context.Background(), "  ")
	if !errors.Is(err, validation.ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
	if got := a.only(); got.Event != validation.EventEmptyInput || got.Detail["phase"] != "host_only" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestValidate_TXTTokenIsTrimmed(t *testing.T) {
	// Some DNS providers add trailing whitespace; reject only on real
	// mismatches.
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"  tok\n"}
	a := &recordingAuditor{}
	v, _ := newValidator(r, a)

	if _, err := v.Validate(context.Background(), "acme.example", "tok"); err != nil {
		t.Fatalf("trimmed TXT should match: %v", err)
	}
}

// TestContainsToken_UsesConstantTimePrimitive is a lint-style regression
// for SIN-63126 (F-3). It locks in that the TXT-token equality check in
// validate.go uses crypto/subtle.ConstantTimeCompare and not variable-
// time primitives. LMHost's security bar (AGENTS.md security lens:
// "constant-time comparison for secrets") treats the verification token
// as a secret, so any reintroduction of `==` / `bytes.Equal` /
// `strings.EqualFold` against it must trip this test.
//
// Microsecond-level timing benchmarks are unreliable across CI runners,
// so the regression is a source-level grep — same approach the F-3
// ticket calls out as sufficient.
func TestContainsToken_UsesConstantTimePrimitive(t *testing.T) {
	body, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatalf("read validate.go: %v", err)
	}
	src := string(body)

	if !strings.Contains(src, "subtle.ConstantTimeCompare") {
		t.Fatalf("validate.go must use crypto/subtle.ConstantTimeCompare for TXT token equality (SIN-63126)")
	}

	// Carve out containsToken's body and reject variable-time primitives
	// inside it. We do not enforce the wider file because other equality
	// checks (e.g. host string compare in tests, audit detail strings)
	// legitimately use ==.
	fnRe := regexp.MustCompile(`(?s)func containsToken\([^)]*\)\s*bool\s*\{(.*?)\n\}`)
	m := fnRe.FindStringSubmatch(src)
	if len(m) < 2 {
		t.Fatalf("could not locate containsToken in validate.go")
	}
	fnBody := m[1]
	for _, banned := range []string{
		"bytes.Equal",
		"strings.EqualFold",
		// Token equality with `==` is the original regression. We allow
		// `len(...) == N` style guards by checking the more specific
		// `== expected` / `== want` patterns instead of bare `==`.
		"== expected",
		"== want",
	} {
		if strings.Contains(fnBody, banned) {
			t.Fatalf("containsToken must not use %q for token equality (SIN-63126); got:\n%s", banned, fnBody)
		}
	}
}
