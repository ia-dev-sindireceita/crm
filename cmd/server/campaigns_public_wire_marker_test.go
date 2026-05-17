package main

// SIN-62982 — wire-level tests for the marker-signing-key env parser
// and the assembleCampaignHandlerWithMarker variant. The marker
// primitive itself is exercised in internal/campaigns/marker_test.go;
// these tests pin the composition root: that we accept both base64
// alphabets, reject too-short keys, and gate the handler construction
// on the parsed key.

import (
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/campaigns"
)

func TestReadMarkerSigningKey(t *testing.T) {
	t.Parallel()
	good32 := make([]byte, 32)
	for i := range good32 {
		good32[i] = byte(i + 1)
	}
	short := make([]byte, 16)
	for i := range short {
		short[i] = byte(i + 1)
	}
	cases := []struct {
		name string
		env  string
		want []byte // nil = expect zero MarkerKey
	}{
		{name: "unset", env: "", want: nil},
		{name: "whitespace only", env: "   ", want: nil},
		{name: "non-base64", env: "@@not-base64@@", want: nil},
		{name: "too short", env: base64.StdEncoding.EncodeToString(short), want: nil},
		{name: "std padded", env: base64.StdEncoding.EncodeToString(good32), want: good32},
		{name: "std raw", env: base64.RawStdEncoding.EncodeToString(good32), want: good32},
		{name: "url padded", env: base64.URLEncoding.EncodeToString(good32), want: good32},
		{name: "url raw", env: base64.RawURLEncoding.EncodeToString(good32), want: good32},
		{name: "padding tolerated", env: "  " + base64.StdEncoding.EncodeToString(good32) + "  ", want: good32},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readMarkerSigningKey(func(string) string { return tc.env })
			if tc.want == nil {
				if got.HasValue() {
					t.Fatalf("readMarkerSigningKey returned non-nil key %x, want zero", []byte(got))
				}
				return
			}
			if !got.HasValue() {
				t.Fatalf("readMarkerSigningKey returned zero key, want %x", tc.want)
			}
			if string(got) != string(tc.want) {
				t.Fatalf("decoded key mismatch: got %x, want %x", []byte(got), tc.want)
			}
		})
	}
}

func TestAssembleCampaignHandlerWithMarker_HappyPath(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	key := campaigns.MarkerKey(strings.Repeat("a", 32))
	h, err := assembleCampaignHandlerWithMarker(repo, []string{"wa.me"}, true, key, slog.Default())
	if err != nil {
		t.Fatalf("assembleCampaignHandlerWithMarker: %v", err)
	}
	if h == nil {
		t.Fatalf("assembleCampaignHandlerWithMarker returned nil handler")
	}
}

func TestAssembleCampaignHandlerWithMarker_NilKeyStillBuilds(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	h, err := assembleCampaignHandlerWithMarker(repo, []string{"wa.me"}, true, nil, slog.Default())
	if err != nil {
		t.Fatalf("assembleCampaignHandlerWithMarker(nil key) err = %v, want nil (handler must boot with legacy unsigned markers)", err)
	}
	if h == nil {
		t.Fatalf("handler is nil")
	}
}
