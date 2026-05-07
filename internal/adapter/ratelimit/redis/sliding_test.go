package redis_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	rladapter "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
)

// fakeScripter is an in-memory replacement for the go-redis Scripter
// surface. It records EVAL inputs and returns a canned slice of
// responses in order — one entry per call. Tests inject the responses
// they expect; calling more times than expected fails the test.
type fakeScripter struct {
	responses []scriptResult
	calls     int32
	loadCalls int32
	loadResp  string
	loadErr   error

	gotKeys [][]string
	gotArgs [][]interface{}
	gotSha  []string
	usedSha int32
}

type scriptResult struct {
	val interface{}
	err error
}

func (f *fakeScripter) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *goredis.Cmd {
	cmd := goredis.NewCmd(ctx, "EVAL")
	idx := atomic.AddInt32(&f.calls, 1) - 1
	f.gotKeys = append(f.gotKeys, keys)
	f.gotArgs = append(f.gotArgs, args)
	f.gotSha = append(f.gotSha, "")
	if int(idx) >= len(f.responses) {
		cmd.SetErr(errors.New("fakeScripter: more EVAL calls than responses configured"))
		return cmd
	}
	r := f.responses[idx]
	if r.err != nil {
		cmd.SetErr(r.err)
		return cmd
	}
	cmd.SetVal(r.val)
	return cmd
}

func (f *fakeScripter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *goredis.Cmd {
	cmd := goredis.NewCmd(ctx, "EVALSHA")
	idx := atomic.AddInt32(&f.calls, 1) - 1
	atomic.AddInt32(&f.usedSha, 1)
	f.gotKeys = append(f.gotKeys, keys)
	f.gotArgs = append(f.gotArgs, args)
	f.gotSha = append(f.gotSha, sha1)
	if int(idx) >= len(f.responses) {
		cmd.SetErr(errors.New("fakeScripter: more EVALSHA calls than responses configured"))
		return cmd
	}
	r := f.responses[idx]
	if r.err != nil {
		cmd.SetErr(r.err)
		return cmd
	}
	cmd.SetVal(r.val)
	return cmd
}

func (f *fakeScripter) ScriptLoad(ctx context.Context, script string) *goredis.StringCmd {
	cmd := goredis.NewStringCmd(ctx, "SCRIPT")
	atomic.AddInt32(&f.loadCalls, 1)
	if f.loadErr != nil {
		cmd.SetErr(f.loadErr)
		return cmd
	}
	cmd.SetVal(f.loadResp)
	return cmd
}

func newSlidingFromFake(f *fakeScripter, prefix string, now time.Time, ids []string) *rladapter.SlidingWindow {
	idx := 0
	idFn := func() string {
		v := ids[idx]
		idx++
		return v
	}
	return rladapter.New(f, prefix).
		WithClock(func() time.Time { return now }).
		WithIDFunc(idFn)
}

func TestSlidingWindow_AllowedHit(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1_000_000)
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(1), int64(0)}},
		},
		loadResp: "deadbeef",
	}
	s := newSlidingFromFake(f, "auth:", now, []string{"id-1"})

	allowed, retry, err := s.Allow(context.Background(), "login:ip:1.2.3.4", time.Minute, 5)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !allowed {
		t.Fatal("allowed = false, want true")
	}
	if retry != 0 {
		t.Fatalf("retry = %v, want 0", retry)
	}
	// Cold-cache path uses Eval, not EvalSha.
	if atomic.LoadInt32(&f.usedSha) != 0 {
		t.Fatal("expected first call to use Eval, not EvalSha")
	}
	if len(f.gotKeys) != 1 || f.gotKeys[0][0] != "auth:login:ip:1.2.3.4" {
		t.Fatalf("script keys = %v, want [auth:login:ip:1.2.3.4]", f.gotKeys)
	}
	wantArgs := []interface{}{"1000000", "60000", "5", "id-1"}
	if !equalArgs(f.gotArgs[0], wantArgs) {
		t.Fatalf("script args = %v, want %v", f.gotArgs[0], wantArgs)
	}
	if atomic.LoadInt32(&f.loadCalls) != 1 {
		t.Fatalf("ScriptLoad calls = %d, want 1", atomic.LoadInt32(&f.loadCalls))
	}
}

func TestSlidingWindow_ThrottledReturnsRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(2_000_000)
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(0), int64(45_000)}}, // 45 seconds
		},
		loadResp: "abc",
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"id-x"})

	allowed, retry, err := s.Allow(context.Background(), "login:email:foo@bar", time.Minute, 5)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if allowed {
		t.Fatal("allowed = true, want false")
	}
	if retry != 45*time.Second {
		t.Fatalf("retry = %v, want 45s", retry)
	}
}

func TestSlidingWindow_UsesEvalShaAfterFirstCall(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(1), int64(0)}}, // first call: Eval
			{val: []interface{}{int64(1), int64(0)}}, // second call: EvalSha
		},
		loadResp: "cached-sha",
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"a", "b"})

	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #1: %v", err)
	}
	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #2: %v", err)
	}
	if got := atomic.LoadInt32(&f.usedSha); got != 1 {
		t.Fatalf("EvalSha calls = %d, want 1 (cold first, warm second)", got)
	}
	if f.gotSha[1] != "cached-sha" {
		t.Fatalf("second call sha = %q, want cached-sha", f.gotSha[1])
	}
}

func TestSlidingWindow_NoScriptFallsBackToEval(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	noscript := errors.New("NOSCRIPT No matching script. Please use EVAL.")
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(1), int64(0)}}, // first Eval
			{err: noscript},                          // EvalSha fails
			{val: []interface{}{int64(1), int64(0)}}, // Eval fallback succeeds
		},
		loadResp: "sha-1",
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"a", "b", "c"})

	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #1: %v", err)
	}
	// The second Allow should: try EvalSha → NOSCRIPT → fall back to
	// Eval. Total of 2 underlying calls.
	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #2: %v", err)
	}
	if got := atomic.LoadInt32(&f.calls); got != 3 {
		t.Fatalf("total calls = %d, want 3 (Eval, EvalSha-NOSCRIPT, Eval-fallback)", got)
	}
}

func TestSlidingWindow_NonNoScriptErrorPropagates(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	infra := errors.New("connection refused")
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(1), int64(0)}},
			{err: infra},
		},
		loadResp: "sha",
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"a", "b"})

	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #1: %v", err)
	}
	_, _, err := s.Allow(context.Background(), "k", time.Second, 1)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want wrapping infra", err)
	}
	if !strings.Contains(err.Error(), "redis/ratelimit") {
		t.Fatalf("err = %q, want package prefix", err)
	}
}

func TestSlidingWindow_ColdEvalErrorWraps(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	infra := errors.New("redis down")
	f := &fakeScripter{
		responses: []scriptResult{
			{err: infra},
		},
		loadResp: "sha",
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"a"})

	_, _, err := s.Allow(context.Background(), "k", time.Second, 1)
	if err == nil || !errors.Is(err, infra) {
		t.Fatalf("err = %v, want wrap of infra", err)
	}
}

func TestSlidingWindow_ScriptLoadFailureDoesNotBreakHotPath(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{int64(1), int64(0)}}, // Eval succeeds
			{val: []interface{}{int64(1), int64(0)}}, // second Allow: Eval again because sha never cached
		},
		loadErr: errors.New("script load denied"),
	}
	s := newSlidingFromFake(f, "rl:", now, []string{"a", "b"})

	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #1: %v", err)
	}
	if _, _, err := s.Allow(context.Background(), "k", time.Second, 1); err != nil {
		t.Fatalf("Allow #2: %v", err)
	}
	if got := atomic.LoadInt32(&f.usedSha); got != 0 {
		t.Fatalf("EvalSha calls = %d, want 0 (sha never cached)", got)
	}
}

func TestSlidingWindow_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	f := &fakeScripter{loadResp: "sha"}
	s := newSlidingFromFake(f, "rl:", time.UnixMilli(0), []string{"a"})

	cases := []struct {
		name   string
		key    string
		window time.Duration
		max    int
	}{
		{"empty key", "", time.Second, 1},
		{"zero window", "k", 0, 1},
		{"negative window", "k", -time.Second, 1},
		{"zero max", "k", time.Second, 0},
		{"negative max", "k", time.Second, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.Allow(context.Background(), tc.key, tc.window, tc.max)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
	if atomic.LoadInt32(&f.calls) != 0 {
		t.Fatal("EVAL must not be invoked on bad inputs")
	}
}

func TestSlidingWindow_DecodesStringNumbers(t *testing.T) {
	t.Parallel()
	// Some redis clients/proxies surface integers as strings. Make
	// sure we tolerate that shape.
	now := time.UnixMilli(0)
	f := &fakeScripter{
		responses: []scriptResult{
			{val: []interface{}{"1", "0"}},
		},
		loadResp: "sha",
	}
	s := newSlidingFromFake(f, "", now, []string{"a"})

	allowed, retry, err := s.Allow(context.Background(), "k", time.Second, 1)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("allowed=%v retry=%v, want true / 0", allowed, retry)
	}
}

func TestSlidingWindow_RejectsMalformedResult(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(0)
	cases := []struct {
		name string
		val  interface{}
	}{
		{"wrong outer type", "not a slice"},
		{"too few", []interface{}{int64(1)}},
		{"non-numeric allowed", []interface{}{1.5, int64(0)}},
		{"non-numeric retry", []interface{}{int64(1), 1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScripter{
				responses: []scriptResult{{val: tc.val}},
				loadResp:  "sha",
			}
			s := newSlidingFromFake(f, "", now, []string{"a"})
			_, _, err := s.Allow(context.Background(), "k", time.Second, 1)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "decode") {
				t.Fatalf("err = %q, want decode prefix", err)
			}
		})
	}
}

func TestSlidingWindow_NilClient(t *testing.T) {
	t.Parallel()
	if got := rladapter.New(nil, "rl:"); got != nil {
		t.Fatal("New(nil) should return nil")
	}
}

// equalArgs compares two arg slices defensively for the few
// representable shapes (string + int + int64).
func equalArgs(got, want []interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
