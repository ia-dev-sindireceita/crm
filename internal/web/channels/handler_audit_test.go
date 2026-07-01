package channels_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// ---- recording auditor (captures the emitted privilege-event lines) ----

type auditLine struct {
	kind    string // "grant" | "revoke" | "restricted"
	actor   uuid.UUID
	tenant  uuid.UUID
	channel uuid.UUID
	user    uuid.UUID
	from    bool
	to      bool
}

type recordingAuditor struct{ lines []auditLine }

func (r *recordingAuditor) ChannelAccessGranted(_ context.Context, actor, tenant, ch, user uuid.UUID) {
	r.lines = append(r.lines, auditLine{kind: "grant", actor: actor, tenant: tenant, channel: ch, user: user})
}

func (r *recordingAuditor) ChannelAccessRevoked(_ context.Context, actor, tenant, ch, user uuid.UUID) {
	r.lines = append(r.lines, auditLine{kind: "revoke", actor: actor, tenant: tenant, channel: ch, user: user})
}

func (r *recordingAuditor) ChannelRestrictedChanged(_ context.Context, actor, tenant, ch uuid.UUID, from, to bool) {
	r.lines = append(r.lines, auditLine{kind: "restricted", actor: actor, tenant: tenant, channel: ch, from: from, to: to})
}

func (r *recordingAuditor) only(kind string) []auditLine {
	var out []auditLine
	for _, l := range r.lines {
		if l.kind == kind {
			out = append(out, l)
		}
	}
	return out
}

// newAuditHandler builds a handler wired with a fixed actor + recording
// auditor. It cannot reuse newHandler (that omits UserID/Audit), so it
// assembles Deps directly — the audit path needs both a resolvable actor
// and the sink.
func newAuditHandler(t *testing.T, repo *fakeRepo, acc *fakeAccess, aud webchannels.AccessAuditor, actor uuid.UUID) http.Handler {
	t.Helper()
	h, err := webchannels.New(webchannels.Deps{
		Channels: repo,
		Access:   acc,
		UserID:   func(*http.Request) uuid.UUID { return actor },
		Audit:    aud,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// TestUpdate_EmitsAuditLine is the AC table: each of the three access-change
// mutations reached through POST /settings/channels/{id} emits exactly one
// audit line carrying actor, tenant, channel and the before/after payload.
func TestUpdate_EmitsAuditLine(t *testing.T) {
	actor := uuid.New()
	userA := uuid.New()

	cases := []struct {
		name            string
		startRestricted bool
		startGrants     []uuid.UUID
		formUsers       []string
		formRestricted  bool
		want            auditLine
	}{
		{
			name:        "grant adds a user to the roster",
			startGrants: nil,
			formUsers:   []string{userA.String()},
			want:        auditLine{kind: "grant", user: userA},
		},
		{
			name:        "revoke removes a user from the roster",
			startGrants: []uuid.UUID{userA},
			formUsers:   nil,
			want:        auditLine{kind: "revoke", user: userA},
		},
		{
			name:            "restricted flip open to restricted",
			startRestricted: false,
			formUsers:       nil,
			formRestricted:  true,
			want:            auditLine{kind: "restricted", from: false, to: true},
		},
		{
			name:            "restricted flip restricted to open",
			startRestricted: true,
			formUsers:       nil,
			formRestricted:  false,
			want:            auditLine{kind: "restricted", from: true, to: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
			ch := mkChannel(t, repo, "whatsapp", "+5511900000000", "Suporte", true)
			ch.Restricted = tc.startRestricted
			acc.grants[ch.ID] = tc.startGrants

			aud := &recordingAuditor{}
			mux := newAuditHandler(t, repo, acc, aud, actor)

			form := url.Values{"name": {"Suporte"}}
			for _, u := range tc.formUsers {
				form.Add("user_ids", u)
			}
			if tc.formRestricted {
				form.Set("restricted", "true")
			}
			rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200\nbody=%s", rec.Code, rec.Body.String())
			}

			got := aud.only(tc.want.kind)
			if len(got) != 1 {
				t.Fatalf("expected exactly one %q line, got %d (%+v)", tc.want.kind, len(got), aud.lines)
			}
			line := got[0]
			if line.actor != actor {
				t.Errorf("actor = %s, want %s", line.actor, actor)
			}
			if line.tenant != testTenant.ID {
				t.Errorf("tenant = %s, want %s", line.tenant, testTenant.ID)
			}
			if line.channel != ch.ID {
				t.Errorf("channel = %s, want %s", line.channel, ch.ID)
			}
			switch tc.want.kind {
			case "grant", "revoke":
				if line.user != tc.want.user {
					t.Errorf("user = %s, want %s", line.user, tc.want.user)
				}
			case "restricted":
				if line.from != tc.want.from || line.to != tc.want.to {
					t.Errorf("restricted from/to = %v/%v, want %v/%v", line.from, line.to, tc.want.from, tc.want.to)
				}
			}
		})
	}
}

// TestUpdate_NoOpRestrictedEmitsNothing pins that re-submitting the edit
// form without flipping the flag records no restricted line — the trail
// captures privilege *changes*, not idempotent saves.
func TestUpdate_NoOpRestrictedEmitsNothing(t *testing.T) {
	actor := uuid.New()
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "whatsapp", "+5511900000000", "Suporte", true)
	ch.Restricted = true

	aud := &recordingAuditor{}
	mux := newAuditHandler(t, repo, acc, aud, actor)

	form := url.Values{"name": {"Suporte"}, "restricted": {"true"}}
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if lines := aud.only("restricted"); len(lines) != 0 {
		t.Fatalf("no-op flip emitted %d restricted lines, want 0", len(lines))
	}
}

// TestCreate_EmitsGrantAndRestrictedLines pins the create path: a new
// restricted channel with an initial roster emits one grant per user and a
// single open→restricted line (before is empty / false for a fresh channel).
func TestCreate_EmitsGrantAndRestrictedLines(t *testing.T) {
	actor := uuid.New()
	userA, userB := uuid.New(), uuid.New()
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))

	aud := &recordingAuditor{}
	mux := newAuditHandler(t, repo, acc, aud, actor)

	form := url.Values{
		"name":        {"Novo"},
		"channel_key": {"whatsapp"},
		"identity":    {"+5511911112222"},
		"restricted":  {"true"},
	}
	form.Add("user_ids", userA.String())
	form.Add("user_ids", userB.String())

	rec := do(t, mux, http.MethodPost, "/settings/channels", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200\nbody=%s", rec.Code, rec.Body.String())
	}

	grants := aud.only("grant")
	if len(grants) != 2 {
		t.Fatalf("create emitted %d grant lines, want 2 (%+v)", len(grants), aud.lines)
	}
	seen := map[uuid.UUID]bool{}
	for _, g := range grants {
		if g.actor != actor || g.tenant != testTenant.ID {
			t.Errorf("grant line actor/tenant = %s/%s", g.actor, g.tenant)
		}
		seen[g.user] = true
	}
	if !seen[userA] || !seen[userB] {
		t.Fatalf("grant lines missing a user: %+v", grants)
	}
	if restr := aud.only("restricted"); len(restr) != 1 || restr[0].from != false || restr[0].to != true {
		t.Fatalf("create restricted line = %+v, want one false→true", restr)
	}
	if rev := aud.only("revoke"); len(rev) != 0 {
		t.Fatalf("create emitted %d revoke lines, want 0", len(rev))
	}
}

// TestUpdate_NoAuditorIsNoOp guards the fail-soft wiring path: with a nil
// Auditor the mutation still succeeds and nothing panics.
func TestUpdate_NoAuditorIsNoOp(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "whatsapp", "+5511900000000", "Suporte", true)

	// newHandler wires neither UserID nor Audit — the audit helpers must
	// short-circuit without touching the sink.
	mux := newHandler(t, repo, acc)
	form := url.Values{"name": {"Renomeado"}, "restricted": {"true"}}
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if repo.renamed[ch.ID] != "Renomeado" {
		t.Fatalf("update did not apply without an auditor")
	}
}
