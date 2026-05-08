package hibp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/iam/password"
)

// TestClient_IsPwned_HitAndMiss covers the k-anonymity round-trip.
//
//   - Hit: server returns the suffix with a non-zero count → IsPwned=true.
//   - Miss: server returns a single padding line (suffix=0) → IsPwned=false.
//
// Verifies that the prefix the client sends is exactly the first 5 hex
// chars of the uppercased SHA-1 — the contract HIBP enforces.
func TestClient_IsPwned_HitAndMiss(t *testing.T) {
	t.Parallel()

	hitPwd := "Password123!"
	hitSuffix := suffixOf(hitPwd)
	hitPrefix := prefixOf(hitPwd)

	missPwd := "definitely-not-in-corpus-12345"
	missPrefix := prefixOf(missPwd)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/range/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Error("User-Agent missing on HIBP request")
		}
		switch r.URL.Path[len("/range/"):] {
		case hitPrefix:
			fmt.Fprintf(w, "%s:42\r\nAAAA0000000000000000000000000000000:0\r\n", hitSuffix)
		case missPrefix:
			fmt.Fprintf(w, "BBBB1111111111111111111111111111111:0\r\n")
		default:
			t.Errorf("unexpected prefix %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL

	hit, err := c.IsPwned(context.Background(), hitPwd)
	if err != nil {
		t.Fatalf("hit: err=%v", err)
	}
	if !hit {
		t.Fatalf("hit: IsPwned=false, want true")
	}

	miss, err := c.IsPwned(context.Background(), missPwd)
	if err != nil {
		t.Fatalf("miss: err=%v", err)
	}
	if miss {
		t.Fatalf("miss: IsPwned=true, want false")
	}
}

// TestClient_NetworkError_BreakerTrips — five consecutive failures trip
// the breaker; the sixth call short-circuits to ErrPwnedCheckUnavailable
// without touching the network.
func TestClient_NetworkError_BreakerTrips(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	for i := 0; i < defaultBreakerThreshold; i++ {
		_, err := c.IsPwned(context.Background(), "anything-12c")
		if !errors.Is(err, password.ErrPwnedCheckUnavailable) {
			t.Fatalf("call %d: err=%v want ErrPwnedCheckUnavailable", i, err)
		}
	}
	got := int(calls.Load())
	if got != defaultBreakerThreshold {
		t.Fatalf("HTTP calls before trip: %d want %d", got, defaultBreakerThreshold)
	}
	// One more — must short-circuit.
	_, err := c.IsPwned(context.Background(), "anything-12c")
	if !errors.Is(err, password.ErrPwnedCheckUnavailable) {
		t.Fatalf("post-trip err=%v want ErrPwnedCheckUnavailable", err)
	}
	if int(calls.Load()) != defaultBreakerThreshold {
		t.Fatalf("post-trip HTTP calls: %d want %d (no extra calls after trip)",
			calls.Load(), defaultBreakerThreshold)
	}
}

// TestClient_HalfOpen_ProbeRecovers — after the cool-down, a single probe
// is allowed; on success the breaker closes and traffic flows again.
func TestClient_HalfOpen_ProbeRecovers(t *testing.T) {
	t.Parallel()

	var fail atomic.Bool
	fail.Store(true)
	hitPwd := "qwerty12345!"
	hitSuffix := suffixOf(hitPwd)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%s:1\r\n", hitSuffix)
	}))
	defer srv.Close()

	c := New()
	c.BaseURL = srv.URL
	// Trip the breaker.
	for i := 0; i < defaultBreakerThreshold; i++ {
		_, _ = c.IsPwned(context.Background(), hitPwd)
	}
	// Force the cool-down to elapse without sleeping the test.
	c.breaker.now = func() time.Time { return time.Now().Add(2 * defaultBreakerCooldown) }
	fail.Store(false)
	hit, err := c.IsPwned(context.Background(), hitPwd)
	if err != nil {
		t.Fatalf("half-open probe: err=%v", err)
	}
	if !hit {
		t.Fatalf("half-open probe: IsPwned=false want true")
	}
	// Now closed — second call also flows.
	if _, err := c.IsPwned(context.Background(), hitPwd); err != nil {
		t.Fatalf("post-recover: err=%v", err)
	}
}

// TestClient_NilHTTPClient_FailsClosed protects against the zero-value
// trap: a Client with no HTTPClient must not call rt.Do(nil) — it must
// return ErrPwnedCheckUnavailable so the policy fall-through fires.
func TestClient_NilHTTPClient_FailsClosed(t *testing.T) {
	t.Parallel()
	c := &Client{}
	_, err := c.IsPwned(context.Background(), "anything-12c")
	if !errors.Is(err, password.ErrPwnedCheckUnavailable) {
		t.Fatalf("err=%v want ErrPwnedCheckUnavailable", err)
	}
}

// TestClient_NilReceiver — defensive: callers may compose with nil.
func TestClient_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *Client
	_, err := c.IsPwned(context.Background(), "anything-12c")
	if !errors.Is(err, password.ErrPwnedCheckUnavailable) {
		t.Fatalf("err=%v want ErrPwnedCheckUnavailable", err)
	}
}

// TestLocalList_Bundled — the embedded list parses cleanly and matches
// known top-N entries (e.g. "Password123!" SHA-1 prefix).
func TestLocalList_Bundled(t *testing.T) {
	t.Parallel()
	ll, err := NewLocalList()
	if err != nil {
		t.Fatalf("NewLocalList: %v", err)
	}
	if ll.Size() < 5 {
		t.Fatalf("bundled list too small: %d entries", ll.Size())
	}
	hit, err := ll.IsPwned(context.Background(), "password")
	if err != nil {
		t.Fatalf("IsPwned: %v", err)
	}
	if !hit {
		t.Fatalf("'password' not in bundled list — list integrity broken")
	}
	miss, err := ll.IsPwned(context.Background(), "this-is-not-in-the-bundled-list-1234567890")
	if err != nil {
		t.Fatalf("IsPwned: %v", err)
	}
	if miss {
		t.Fatalf("unrelated password matched bundled list — collision impossible")
	}
}

// TestLocalList_RejectsMalformed — a bundled file with junk lines fails
// at construction so a deploy with a broken corpus file refuses to start
// instead of silently accepting every password.
func TestLocalList_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"short":   []byte("ABC\n"),
		"non-hex": []byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ\n"),
		"long":    []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"),
	}
	for name, in := range cases {
		in := in
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := newLocalListFromBytes(in); err == nil {
				t.Fatalf("expected error for malformed bundle, got nil")
			}
		})
	}
}

// TestLocalList_NilSafe — the policy may be wired without a local list;
// a nil receiver MUST return (false, nil) and not panic.
func TestLocalList_NilSafe(t *testing.T) {
	t.Parallel()
	var ll *LocalList
	if ll.Size() != 0 {
		t.Fatalf("nil list Size() != 0")
	}
	hit, err := ll.IsPwned(context.Background(), "anything")
	if hit || err != nil {
		t.Fatalf("nil list IsPwned: hit=%v err=%v", hit, err)
	}
}

// TestBreaker_RaceSafety — the breaker is shared across concurrent
// requests; this test sanity-checks that allow/recordFailure under
// contention do not panic or violate the failure-count invariant.
func TestBreaker_RaceSafety(t *testing.T) {
	t.Parallel()
	b := newBreaker(50, 100*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if b.allow() {
					if j%3 == 0 {
						b.recordSuccess()
					} else {
						b.recordFailure()
					}
				}
			}
		}()
	}
	wg.Wait()
}

func suffixOf(plain string) string {
	sum := sha1.Sum([]byte(plain))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[5:]
}

func prefixOf(plain string) string {
	sum := sha1.Sum([]byte(plain))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:5]
}
