package main

// SIN-65180 — compact conversation-context header guard. At the default
// 1440 3-pane layout the conversation column is only ~760px wide; the four
// inline uppercase per-facet subtitles (CONTATO / CANAL / ETAPA DO FUNIL /
// ATRIBUIÇÃO) plus their values overflow it, wrapping the strip to 2 rows
// (~91px) and missing the parent AC of ≤64px / 4 facetas numa linha. The
// fix moves .conversation-context__subtitle into the screen-reader layer
// (clip rect / position:absolute) — the badge, funnel pill, and assignee
// chip carry recognition and each <section> keeps its aria-label. This
// test fails if the subtitle drifts back to a visible (non-clipped) style,
// which would re-inflate the strip.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestInboxStylesheet_ContextSubtitleScreenReaderOnly(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/inbox.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/inbox.css must exist", rec.Code)
	}
	body := rec.Body.String()

	// Isolate the .conversation-context__subtitle rule body so a clip rule
	// living in some other selector cannot satisfy the assertion.
	re := regexp.MustCompile(`(?s)\.conversation-context__subtitle\s*\{(.*?)\}`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("inbox.css missing .conversation-context__subtitle rule")
	}
	rule := m[1]

	// The sr-only clip pattern: pulled out of flow and clipped to nothing.
	for _, needle := range []string{
		"position: absolute",
		"clip-path: inset(50%)",
	} {
		if !strings.Contains(rule, needle) {
			t.Errorf(".conversation-context__subtitle missing sr-only fragment %q — labels must stay clipped so the compact strip holds one row", needle)
		}
	}

	// Guard against a regression to the old visible uppercase label, which
	// is what re-inflated the strip to 2 rows at 1440 (SIN-65180).
	if strings.Contains(rule, "text-transform: uppercase") {
		t.Errorf(".conversation-context__subtitle is visible again (text-transform: uppercase) — it must be screen-reader-only to keep the strip ≤64px / 4 facets on one line")
	}
}
