package httpapi_test

// SIN-62985 — coverage for the info-level boot log emitted by
// NewTrustedRealIP when TRUSTED_PROXY_CIDRS contains invalid entries.
// The SIN-62978 behavioural tests in trusted_realip_test.go pin the
// trust-gate semantics; this file pins the operator-discoverability
// contract added in SIN-62985 (doc-comment promise that drops are
// surfaced).

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
)

// captureLogger returns a slog.Logger that writes JSON records into buf
// at info+. JSON makes the assertions resilient against minor format
// drift (key order, escaping) compared to the human-readable text
// handler.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h)
}

// readDropRecord returns the first JSON-encoded log record whose msg
// matches "trusted_proxy: dropped invalid CIDR entries". Empty map +
// false if no such record is present.
func readDropRecord(t *testing.T, buf *bytes.Buffer) (map[string]any, bool) {
	t.Helper()
	scanner := buf.Bytes()
	for _, line := range bytes.Split(scanner, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("non-JSON log line: %q (err %v)", line, err)
		}
		if msg, _ := rec["msg"].(string); msg == "trusted_proxy: dropped invalid CIDR entries" {
			return rec, true
		}
	}
	return nil, false
}

func TestParseTrustedProxies_ReturnsDroppedEntries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		raw         string
		wantCIDRs   int
		wantDropped []string
	}{
		{name: "empty", raw: "", wantCIDRs: 0, wantDropped: nil},
		{name: "whitespace only", raw: "   ", wantCIDRs: 0, wantDropped: nil},
		{name: "all valid", raw: "10.0.0.0/8,192.168.1.0/24", wantCIDRs: 2, wantDropped: nil},
		{name: "single bogus", raw: "bogus", wantCIDRs: 0, wantDropped: []string{"bogus"}},
		{name: "mixed", raw: "10.0.0.0/8,bogus,2001:db8::/32,nope", wantCIDRs: 2, wantDropped: []string{"bogus", "nope"}},
		{name: "empty entries ignored", raw: "10.0.0.0/8,,,192.168.1.0/24", wantCIDRs: 2, wantDropped: nil},
		{name: "trailing comma + bad entry", raw: "10.0.0.0/8,bogus,", wantCIDRs: 1, wantDropped: []string{"bogus"}},
		{name: "trimmed bogus", raw: "  bogus  ,  10.0.0.0/8  ", wantCIDRs: 1, wantDropped: []string{"bogus"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotCIDRs, gotDropped := httpapi.ParseTrustedProxiesForTest(tc.raw)
			if gotCIDRs != tc.wantCIDRs {
				t.Errorf("cidr count = %d, want %d", gotCIDRs, tc.wantCIDRs)
			}
			if !reflect.DeepEqual(gotDropped, tc.wantDropped) {
				t.Errorf("dropped = %v, want %v", gotDropped, tc.wantDropped)
			}
		})
	}
}

func TestNewTrustedRealIP_LogsDroppedEntries(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	getenv := func(name string) string {
		if name == httpapi.TrustedProxyEnv {
			return "10.0.0.0/8,bogus,nope"
		}
		return ""
	}
	_ = httpapi.NewTrustedRealIPWithLoggerForTest(getenv, captureLogger(&buf))

	rec, ok := readDropRecord(t, &buf)
	if !ok {
		t.Fatalf("expected one log record about dropped CIDR entries, got: %s", buf.String())
	}
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got := rec["dropped"]; got != "bogus,nope" {
		t.Errorf("dropped = %v, want %q", got, "bogus,nope")
	}
	if got := rec["dropped_count"]; got != float64(2) {
		// JSON numbers decode to float64.
		t.Errorf("dropped_count = %v, want 2", got)
	}
	if got, ok := rec["fellback"].(bool); !ok || got {
		t.Errorf("fellback = %v, want false (at least one valid CIDR remained)", rec["fellback"])
	}
	if got := rec["env"]; got != httpapi.TrustedProxyEnv {
		t.Errorf("env = %v, want %q", got, httpapi.TrustedProxyEnv)
	}
}

func TestNewTrustedRealIP_LogsFellbackTrueWhenEverythingInvalid(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	getenv := func(name string) string {
		if name == httpapi.TrustedProxyEnv {
			return "not-a-cidr,also-not-a-cidr"
		}
		return ""
	}
	_ = httpapi.NewTrustedRealIPWithLoggerForTest(getenv, captureLogger(&buf))

	rec, ok := readDropRecord(t, &buf)
	if !ok {
		t.Fatalf("expected drop log record, got: %s", buf.String())
	}
	if got, ok := rec["fellback"].(bool); !ok || !got {
		t.Errorf("fellback = %v, want true (no valid CIDR survived → defaults applied)", rec["fellback"])
	}
}

func TestNewTrustedRealIP_NoLogWhenAllEntriesValid(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	getenv := func(name string) string {
		if name == httpapi.TrustedProxyEnv {
			return "10.0.0.0/8,192.168.1.0/24"
		}
		return ""
	}
	_ = httpapi.NewTrustedRealIPWithLoggerForTest(getenv, captureLogger(&buf))

	if _, ok := readDropRecord(t, &buf); ok {
		t.Fatalf("unexpected drop log line when all entries valid: %s", buf.String())
	}
	if strings.Contains(buf.String(), "trusted_proxy") {
		t.Fatalf("unexpected trusted_proxy log line: %s", buf.String())
	}
}

func TestNewTrustedRealIP_NoLogWhenEnvUnset(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_ = httpapi.NewTrustedRealIPWithLoggerForTest(func(string) string { return "" }, captureLogger(&buf))

	if _, ok := readDropRecord(t, &buf); ok {
		t.Fatalf("unexpected drop log line when env unset (no drops): %s", buf.String())
	}
}

func TestNewTrustedRealIP_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// nil logger must not panic — the helper falls back to slog.Default.
	// We do not assert on the default's output because that would race
	// with parallel tests; this test only proves the construction path
	// handles nil safely.
	mw := httpapi.NewTrustedRealIPWithLoggerForTest(
		func(name string) string {
			if name == httpapi.TrustedProxyEnv {
				return "bogus"
			}
			return ""
		},
		nil,
	)
	if mw == nil {
		t.Fatalf("nil logger should not yield nil middleware")
	}
}
