package inbox_test

// SIN-65156 — compact-header refinements for the conversation context
// strip. The bulky reassign controls (select + "Atribuir" + "Atribuir a
// mim") now live inside a native <details> so the strip stays a single
// short row: the assignee chip is the always-visible <summary>, the
// controls reveal on demand. The disclosure is CSP-safe (no JS), and the
// section keeps its #conversation-context-assignment swap target so the
// existing HTMX assign flow (SIN-64979) is untouched.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

func TestAssignPartial_CollapsesControlsInDetails(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	targetID := uuid.New()

	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, &stubListAssignable{
		rows: []webinbox.AssignableRow{{UserID: targetID, DisplayName: "Ana Lima"}},
	})

	body := "targetUserID=" + targetID.String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/assign", body, tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	html := rec.Body.String()

	for _, want := range []string{
		// Controls are collapsed into the native disclosure.
		`<details class="conversation-context__assign"`,
		`<summary class="conversation-context__assign-summary"`,
		`class="conversation-context__assign-controls"`,
		`conversation-context__assign-toggle`,
		// The assignee chip + name stay visible in the summary.
		"Ana Lima",
		// The swap target id and HTMX contract are preserved verbatim so
		// the assign flow keeps working after the markup change.
		`id="conversation-context-assignment"`,
		`hx-target="#conversation-context-assignment"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("assign partial missing %q\nbody=%s", want, html)
		}
	}

	// The reassign form (select + buttons) must live INSIDE the <details>,
	// not before it — otherwise the strip never actually collapses.
	detailsAt := strings.Index(html, "<details")
	formAt := strings.Index(html, "conversation-context__assign-form")
	if detailsAt < 0 || formAt < 0 || formAt < detailsAt {
		t.Errorf("reassign form must render inside <details>: detailsAt=%d formAt=%d\nbody=%s", detailsAt, formAt, html)
	}
}
