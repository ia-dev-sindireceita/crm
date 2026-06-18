package walletui_test

// SIN-65103 / Peitho C8 — the wallet balance card severity glyph must
// render the Peitho inline-SVG {{icon}} helper instead of the Unicode
// emoji it shipped with (⛔ / ✅ / ⚠️). Emoji are forbidden in chrome:
// they are inconsistent across platforms and ignore the design tokens.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/walletui"
)

func TestDashboard_RendersSeverityIconNoEmoji(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	avg := int64(400)
	monthly := avg * 30
	tt := []struct {
		name     string
		snap     walletui.DashboardSnapshot
		wantPath string // distinctive sub-path of the expected Lucide icon
	}{
		{
			name: "blocked renders octagon-alert",
			snap: walletui.DashboardSnapshot{
				Balance: 1_000_000, Available: 1_000_000, AvgDailyConsume: avg,
				DunningState: "suspended_full",
			},
			wantPath: `M15.312 2a2 2 0 0 1 1.414.586`,
		},
		{
			name: "critical renders octagon-alert",
			snap: walletui.DashboardSnapshot{
				Balance: monthly * 4 / 100, Available: monthly * 4 / 100, AvgDailyConsume: avg,
			},
			wantPath: `M15.312 2a2 2 0 0 1 1.414.586`,
		},
		{
			name: "warn renders octagon-alert",
			snap: walletui.DashboardSnapshot{
				Balance: monthly * 19 / 100, Available: monthly * 19 / 100, AvgDailyConsume: avg,
			},
			wantPath: `M15.312 2a2 2 0 0 1 1.414.586`,
		},
		{
			name: "ok renders check-circle",
			snap: walletui.DashboardSnapshot{
				Balance: 1_000, Available: 1_000, AvgDailyConsume: 0,
			},
			wantPath: `m9 11 3 3L22 4`,
		},
	}
	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dash := &stubDashboard{snapshot: tc.snap}
			h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
			mux := http.NewServeMux()
			h.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", tenant, false))
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `class="peitho-icon"`) {
				t.Fatalf("balance card glyph not rendered as inline peitho-icon SVG\nbody=%s", body)
			}
			if !strings.Contains(body, tc.wantPath) {
				t.Errorf("expected icon geometry %q in balance card", tc.wantPath)
			}
			for _, g := range []string{"⛔", "✅", "⚠️", "⚠"} {
				if strings.Contains(body, g) {
					t.Errorf("emoji %q leaked into wallet chrome (must use {{icon}})", g)
				}
			}
		})
	}
}
