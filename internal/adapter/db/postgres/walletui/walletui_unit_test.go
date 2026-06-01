package walletui

// SIN-63954 — unit tests for the pure helpers in the walletui adapter.
// These complement the integration tests in
// internal/adapter/db/postgres/walletui_adapter_test.go (postgres_test
// package): they exercise the redaction, CSV-row formatting and
// price-per-token rounding without paying the testpg harness startup
// cost. Co-located here so they live in the same package as the helpers
// and can verify both exported and unexported behaviour.

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	walletuiport "github.com/pericles-luz/crm/internal/web/walletui"
)

func TestExtractLGPDFields_EmptyMetadataReturnsZero(t *testing.T) {
	hash, model, policy := extractLGPDFields(nil)
	if hash != "" || model != "" || policy != uuid.Nil {
		t.Errorf("empty meta: got (%q, %q, %v); want zero values", hash, model, policy)
	}
	hash, model, policy = extractLGPDFields([]byte{})
	if hash != "" || model != "" || policy != uuid.Nil {
		t.Errorf("empty bytes: got (%q, %q, %v); want zero values", hash, model, policy)
	}
}

func TestExtractLGPDFields_MalformedJSONFailsClosed(t *testing.T) {
	hash, model, policy := extractLGPDFields([]byte("{not-json"))
	if hash != "" || model != "" || policy != uuid.Nil {
		t.Errorf("malformed: got (%q, %q, %v); want zero values", hash, model, policy)
	}
}

func TestExtractLGPDFields_NonStringFieldsIgnored(t *testing.T) {
	// numeric conversation_id / model / ai_policy_id values must not
	// crash the decoder — they collapse to empty fields.
	raw := []byte(`{"conversation_id":42,"model":3.14,"ai_policy_id":true}`)
	hash, model, policy := extractLGPDFields(raw)
	if hash != "" || model != "" || policy != uuid.Nil {
		t.Errorf("non-string: got (%q, %q, %v); want zero values", hash, model, policy)
	}
}

func TestExtractLGPDFields_InvalidPolicyUUIDCollapsesToNil(t *testing.T) {
	raw := []byte(`{"ai_policy_id":"not-a-uuid"}`)
	_, _, policy := extractLGPDFields(raw)
	if policy != uuid.Nil {
		t.Errorf("invalid policy uuid: got %v; want uuid.Nil", policy)
	}
}

func TestExtractLGPDFields_HappyPath(t *testing.T) {
	convID := "conv-abc"
	policyID := uuid.New()
	raw := []byte(`{"conversation_id":"` + convID + `","model":"haiku","ai_policy_id":"` + policyID.String() + `"}`)
	hash, model, policy := extractLGPDFields(raw)
	want := func() string {
		sum := sha256.Sum256([]byte(convID))
		return hex.EncodeToString(sum[:])[:16]
	}()
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if model != "haiku" {
		t.Errorf("model = %q, want haiku", model)
	}
	if policy != policyID {
		t.Errorf("policy = %v, want %v", policy, policyID)
	}
}

func TestHashConversationID_EmptyInputReturnsEmpty(t *testing.T) {
	if got := hashConversationID(""); got != "" {
		t.Errorf("hash empty: got %q, want empty", got)
	}
}

func TestHashConversationID_NonEmptyInputIsHex16(t *testing.T) {
	got := hashConversationID("anything")
	if len(got) != 16 {
		t.Errorf("len = %d, want 16", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("not hex: %v", err)
	}
}

func TestStringField_PresenceAndType(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		key  string
		want string
		ok   bool
	}{
		{"present", map[string]any{"a": "x"}, "a", "x", true},
		{"missing", map[string]any{"a": "x"}, "b", "", false},
		{"empty-string", map[string]any{"a": ""}, "a", "", false},
		{"non-string", map[string]any{"a": 7.0}, "a", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stringField(c.m, c.key)
			if got != c.want || ok != c.ok {
				t.Errorf("got (%q, %v); want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestPricePerKToken_Rounding(t *testing.T) {
	cases := []struct {
		name   string
		cents  int
		tokens int64
		want   int
	}{
		{"zero tokens guard", 1500, 0, 0},
		{"negative tokens guard", 1500, -1, 0},
		{"exact division", 1000, 1_000_000, 1},
		{"round up (>= half)", 1500, 1_000_000, 2},   // 1.5 → 2
		{"round down (< half)", 1499, 1_000_000, 1},  // 1.499 → 1
		{"large bundle floor", 14900, 20_000_000, 1}, // 0.745 → 1
		{"mid bundle round", 4900, 5_000_000, 1},     // 0.98 → 1
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pricePerKToken(c.cents, c.tokens); got != c.want {
				t.Errorf("pricePerKToken(%d, %d) = %d, want %d", c.cents, c.tokens, got, c.want)
			}
		})
	}
}

func TestKindsArg_EmptyVsNonEmpty(t *testing.T) {
	if a, b := kindsArg(nil); a != nil || b != nil {
		t.Errorf("nil: (%v, %v), want (nil, nil)", a, b)
	}
	if a, b := kindsArg([]wallet.LedgerKind{}); a != nil || b != nil {
		t.Errorf("empty: (%v, %v), want (nil, nil)", a, b)
	}
	a, b := kindsArg([]wallet.LedgerKind{wallet.KindCommit, wallet.KindGrant})
	got, ok := a.([]string)
	if !ok {
		t.Fatalf("kindsArg arg type = %T, want []string", a)
	}
	if len(got) != 2 || got[0] != "commit" || got[1] != "grant" {
		t.Errorf("kinds = %v, want [commit grant]", got)
	}
	if n, ok := b.(int); !ok || n != 2 {
		t.Errorf("len arg = %v, want 2", b)
	}
}

func TestCsvRow_Format(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	policy := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	v := walletuiport.LedgerEntryView{
		ID:                 id,
		Kind:               wallet.KindCommit,
		Source:             wallet.SourceConsumption,
		Amount:             -100,
		ConversationIDHash: "abc",
		Model:              "haiku",
		PolicyID:           policy,
		ExternalRef:        "wamid",
	}
	row := csvRow(v)
	if row[0] != id.String() {
		t.Errorf("row[0] = %q, want %q", row[0], id.String())
	}
	if row[2] != "commit" || row[3] != "consumption" {
		t.Errorf("kind/source = %q/%q", row[2], row[3])
	}
	if row[4] != "-100" {
		t.Errorf("amount = %q, want -100", row[4])
	}
	if row[7] != policy.String() {
		t.Errorf("policy = %q, want %q", row[7], policy.String())
	}

	v.PolicyID = uuid.Nil
	row = csvRow(v)
	if row[7] != "" {
		t.Errorf("nil policy: row[7] = %q, want empty", row[7])
	}
}

func TestApplyBalanceAfter_RollsBackward(t *testing.T) {
	entries := []walletuiport.LedgerEntryView{
		{Amount: -100},
		{Amount: -200},
		{Amount: 500},
	}
	applyBalanceAfter(entries, 1000)
	wantAfter := []int64{1000, 1100, 1300}
	for i, want := range wantAfter {
		if entries[i].BalanceAfter != want {
			t.Errorf("entries[%d].BalanceAfter = %d, want %d", i, entries[i].BalanceAfter, want)
		}
	}
}

func TestBoundsArgs_ZeroVsNonZero(t *testing.T) {
	zero := walletuiport.LedgerFilter{}
	a, b := boundsArgs(zero.FromOccurredAt, zero.ToOccurredAt)
	if a != nil || b != nil {
		t.Errorf("zero: (%v, %v), want (nil, nil)", a, b)
	}
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a, b = boundsArgs(now, now)
	if a == nil || b == nil {
		t.Errorf("non-zero: (%v, %v), want both non-nil", a, b)
	}
}
