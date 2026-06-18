package master

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGrantRequestTemplates_NoInlineEventHandlers pins SIN-65117 (gap
// in SIN-63977 / SEC-F1): the 4-eyes grant-request templates must NOT
// render any inline `on*="…"` DOM-event-handler attribute. Under the
// strict-CSP policy (script-src 'self' 'nonce-…', no unsafe-inline /
// unsafe-eval) such handlers silently no-op, which had let the
// destructive grant-REJECT form submit with no confirmation guard.
//
// The reject confirmation is now wired via htmx's `hx-confirm`
// attribute, which is read by the external (script-src 'self')
// htmx.min.js — no inline handler and no eval, so it survives strict
// CSP. This test exercises every awaiting render path (default actions
// + the confirm-twice modal) where the forms appear.
func TestGrantRequestTemplates_NoInlineEventHandlers(t *testing.T) {
	t.Parallel()

	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reviewer := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	awaiting := GrantRequest{
		ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		ExternalID:  "req_csp",
		CreatedByID: requester,
		State:       GrantRequestStateAwaiting,
		Kind:        GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "csp conformance reason",
		CreatedAt:   time.Now().UTC(),
	}

	detail := func(stage string) grantRequestDetailData {
		return grantRequestDetailData{
			Request:       awaiting,
			CurrentUserID: reviewer,
			ConfirmStage:  stage,
		}
	}

	cases := []struct {
		name string
		exec func(*bytes.Buffer) error
	}{
		{
			name: "requests_layout",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestsLayoutTmpl.Execute(buf, grantRequestsListData{Requests: []GrantRequest{awaiting}})
			},
		},
		{
			name: "requests_panel",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestsPanelTmpl.Execute(buf, grantRequestsListData{Requests: []GrantRequest{awaiting}})
			},
		},
		{
			name: "detail_layout_actions",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestDetailLayoutTmpl.Execute(buf, detail(""))
			},
		},
		{
			name: "detail_panel_actions",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestDetailPanelTmpl.Execute(buf, detail(""))
			},
		},
		{
			name: "detail_panel_confirm_modal",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestDetailPanelTmpl.Execute(buf, detail("confirm"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.exec(&buf); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			rendered := buf.String()
			if loc := inlineHandlerRE.FindStringIndex(rendered); loc != nil {
				ctxStart := loc[0] - 40
				if ctxStart < 0 {
					ctxStart = 0
				}
				ctxEnd := loc[1] + 80
				if ctxEnd > len(rendered) {
					ctxEnd = len(rendered)
				}
				t.Fatalf("inline event-handler attribute leaked into rendered output:\n  fragment: %s\nstrict-CSP (script-src 'self' 'nonce-…') blocks these at runtime — use htmx attributes (hx-confirm, hx-post) or a nonced/external script instead",
					rendered[ctxStart:ctxEnd])
			}
		})
	}
}

// TestGrantRequestDetail_RejectGuardIsCSPSafe pins that the destructive
// reject form keeps a confirmation guard wired the CSP-safe way: an
// `hx-confirm` attribute (honoured by the external htmx script) rather
// than the previous inline `onsubmit="return confirm(…)"`, which the
// strict-CSP policy refuses to execute. Regression guard for SIN-65117.
func TestGrantRequestDetail_RejectGuardIsCSPSafe(t *testing.T) {
	t.Parallel()
	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reviewer := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	req := GrantRequest{
		ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		ExternalID:  "req_reject_guard",
		CreatedByID: requester,
		State:       GrantRequestStateAwaiting,
		Kind:        GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "reject guard reason",
		CreatedAt:   time.Now().UTC(),
	}
	var buf bytes.Buffer
	if err := grantRequestDetailPanelTmpl.Execute(&buf, grantRequestDetailData{
		Request:       req,
		CurrentUserID: reviewer,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rendered := buf.String()

	if !strings.Contains(rendered, `hx-confirm="Confirmar rejeição da solicitação?"`) {
		t.Fatalf("reject form must carry hx-confirm guard (CSP-safe); body=%s", rendered)
	}
	if strings.Contains(rendered, "onsubmit") {
		t.Fatalf("reject form must not use inline onsubmit (breaks under strict CSP); body=%s", rendered)
	}
	// The reject form + endpoint must still be present — the guard
	// supplements the POST, it does not replace it.
	if !strings.Contains(rendered, `action="/master/grants/requests/`+req.ID.String()+`/reject"`) {
		t.Fatalf("reject form action missing; body=%s", rendered)
	}
}
