package campaigns_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
)

func TestBuildClickToken_NoKeyReturnsBareClickID(t *testing.T) {
	t.Parallel()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	got := campaigns.BuildClickToken(nil, uuid.New(), clickID)
	if got != clickID {
		t.Fatalf("BuildClickToken(nil, …) = %q, want %q", got, clickID)
	}
}

func TestBuildClickToken_KeyedAppendsHMAC(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	got := campaigns.BuildClickToken(key, tenant, clickID)
	if !strings.HasPrefix(got, clickID+".") {
		t.Fatalf("BuildClickToken = %q, want prefix %q.", got, clickID)
	}
	suffix := strings.TrimPrefix(got, clickID+".")
	if len(suffix) != 8 {
		t.Fatalf("hmac suffix len = %d, want 8 (%q)", len(suffix), suffix)
	}
	for _, c := range suffix {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			t.Fatalf("hmac suffix %q contains non-lowercase-hex byte %q", suffix, string(c))
		}
	}
}

func TestBuildClickToken_TenantBindingChangesHMAC(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	t1, t2 := uuid.New(), uuid.New()
	a := campaigns.BuildClickToken(key, t1, clickID)
	b := campaigns.BuildClickToken(key, t2, clickID)
	if a == b {
		t.Fatalf("token for tenant1 == token for tenant2 (%q) — tenant must change the HMAC", a)
	}
}

func TestBuildClickToken_DifferentKeyChangesHMAC(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	a := campaigns.BuildClickToken(campaigns.MarkerKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), tenant, clickID)
	b := campaigns.BuildClickToken(campaigns.MarkerKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), tenant, clickID)
	if a == b {
		t.Fatalf("two distinct keys produced the same token (%q)", a)
	}
}

func TestExtractClickMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		want      campaigns.ParsedClickMarker
		wantFound bool
	}{
		{
			name: "empty body",
			body: "",
		},
		{
			name: "no marker",
			body: "just a normal greeting",
		},
		{
			name:      "legacy uuid marker",
			body:      "Hi [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]",
			want:      campaigns.ParsedClickMarker{ClickID: "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3", Found: true},
			wantFound: true,
		},
		{
			name:      "signed marker",
			body:      "Hi [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3.deadbeef]",
			want:      campaigns.ParsedClickMarker{ClickID: "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3", HMACHex: "deadbeef", Found: true},
			wantFound: true,
		},
		{
			name: "uppercase hmac is rejected by regex",
			// hmac must be lowercase hex; uppercase falls back to legacy
			// parse with the dot left inside the click_id alphabet.
			body:      "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3.DEADBEEF]",
			want:      campaigns.ParsedClickMarker{},
			wantFound: false,
		},
		{
			name:      "first match wins",
			body:      "[crm:firstoken12.aabbccdd] and [crm:secondtoken12]",
			want:      campaigns.ParsedClickMarker{ClickID: "firstoken12", HMACHex: "aabbccdd", Found: true},
			wantFound: true,
		},
		{
			name: "short click_id rejected",
			body: "[crm:abc.deadbeef]",
		},
		{
			name: "whitespace inside ignored",
			body: "[crm:abc 12345.deadbeef]",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := campaigns.ExtractClickMarker(tc.body)
			if got.Found != tc.wantFound {
				t.Fatalf("Found = %v, want %v (got %+v)", got.Found, tc.wantFound, got)
			}
			if got != tc.want {
				t.Fatalf("Parsed = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVerifyClickToken_HappyPath_AcceptsHMACFromBuildClickToken(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	token := campaigns.BuildClickToken(key, tenant, clickID)
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("BuildClickToken returned unsigned token %q with key set", token)
	}
	if ok := campaigns.VerifyClickToken(key, false, tenant, parts[0], parts[1]); !ok {
		t.Fatalf("VerifyClickToken rejected the marker just built (token=%q)", token)
	}
}

func TestVerifyClickToken_RejectsForgedHMAC(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	// 8 lowercase hex chars but not the real digest.
	if ok := campaigns.VerifyClickToken(key, false, tenant, clickID, "deadbeef"); ok {
		t.Fatalf("VerifyClickToken accepted a forged HMAC")
	}
}

func TestVerifyClickToken_RejectsCrossTenantReplay(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	t1, t2 := uuid.New(), uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	token := campaigns.BuildClickToken(key, t1, clickID)
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("BuildClickToken returned unsigned token %q with key set", token)
	}
	if ok := campaigns.VerifyClickToken(key, false, t2, parts[0], parts[1]); ok {
		t.Fatalf("VerifyClickToken accepted a marker minted for tenant1 against tenant2 — HMAC must bind to tenant_id")
	}
}

func TestVerifyClickToken_LegacyMarkerHonoursAllowFlag(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	if ok := campaigns.VerifyClickToken(key, true, tenant, clickID, ""); !ok {
		t.Fatalf("legacy marker with allowLegacy=true should verify")
	}
	if ok := campaigns.VerifyClickToken(key, false, tenant, clickID, ""); ok {
		t.Fatalf("legacy marker with allowLegacy=false should be rejected")
	}
}

func TestVerifyClickToken_NoKeyAndSuffixedMarkerFailsClosed(t *testing.T) {
	t.Parallel()
	// A wire that lost its key cannot verify a marker minted by a
	// sibling process that still has it. Refuse rather than accept.
	if ok := campaigns.VerifyClickToken(nil, true, uuid.New(), "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3", "deadbeef"); ok {
		t.Fatalf("VerifyClickToken accepted suffixed marker with no key — must fail closed")
	}
}

func TestVerifyClickToken_RejectsMalformedHex(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	tenant := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	cases := []string{"zzzzzzzz", "deadbee", "deadbeef00"} // bad hex / wrong length
	for _, suffix := range cases {
		suffix := suffix
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			if ok := campaigns.VerifyClickToken(key, false, tenant, clickID, suffix); ok {
				t.Fatalf("VerifyClickToken accepted malformed suffix %q", suffix)
			}
		})
	}
}

func TestMarkerKey_HasValue(t *testing.T) {
	t.Parallel()
	var zero campaigns.MarkerKey
	if zero.HasValue() {
		t.Fatalf("zero MarkerKey HasValue() = true, want false")
	}
	if !campaigns.MarkerKey("x").HasValue() {
		t.Fatalf("non-empty MarkerKey HasValue() = false, want true")
	}
}
