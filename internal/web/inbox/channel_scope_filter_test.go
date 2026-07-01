package inbox_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubChannelScope is an in-memory webinbox.ChannelScopeUseCase. It
// records the call and returns a canned accessible-channel set so the
// handler's P4 filtering + chip rendering can be asserted without the
// channels domain.
type stubChannelScope struct {
	channels   []webinbox.AccessibleChannel
	err        error
	gotTenant  uuid.UUID
	gotUser    uuid.UUID
	gotGerente bool
	called     bool
}

func (s *stubChannelScope) AccessibleChannels(_ context.Context, tenantID, userID uuid.UUID, isGerente bool) ([]webinbox.AccessibleChannel, error) {
	s.called = true
	s.gotTenant = tenantID
	s.gotUser = userID
	s.gotGerente = isGerente
	return s.channels, s.err
}

func newHandlerWithChannelScope(t *testing.T, summaries webinbox.ListSummariesUseCase, scope webinbox.ChannelScopeUseCase, isGerente bool, userID uuid.UUID) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListSummaries:     summaries,
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "csrf-test-token" },
		UserID:            func(*http.Request) uuid.UUID { return userID },
		ChannelScope:      scope,
		IsGerente:         func(*http.Request) bool { return isGerente },
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

// TestChannelScope_GerenteSeesAllNoFilter: a gerente gets scope nil (no
// channel_id predicate on the read path) but the chip still lists every
// accessible channel.
func TestChannelScope_GerenteSeesAllNoFilter(t *testing.T) {
	t.Parallel()
	chA, chB := uuid.New(), uuid.New()
	scope := &stubChannelScope{channels: []webinbox.AccessibleChannel{
		{ID: chA, DisplayName: "Suporte"},
		{ID: chB, DisplayName: "Vendas"},
	}}
	summaries := &stubSummaries{}
	h := newHandlerWithChannelScope(t, summaries, scope, true, uuid.New())
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
	}
	if !scope.gotGerente {
		t.Error("ChannelScope not told the caller is a gerente")
	}
	if in := summaries.input(); in.ChannelScope != nil {
		t.Errorf("gerente ChannelScope = %v, want nil (see all)", *in.ChannelScope)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="channel_id"`, "Suporte", "Vendas", "Todas as instâncias"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestChannelScope_AtendenteRestrictedToAccessible: an atendente gets a
// non-nil scope carrying exactly the accessible ids.
func TestChannelScope_AtendenteRestrictedToAccessible(t *testing.T) {
	t.Parallel()
	chA := uuid.New()
	scope := &stubChannelScope{channels: []webinbox.AccessibleChannel{
		{ID: chA, DisplayName: "Suporte"},
	}}
	summaries := &stubSummaries{}
	h := newHandlerWithChannelScope(t, summaries, scope, false, uuid.New())
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
	}
	in := summaries.input()
	if in.ChannelScope == nil {
		t.Fatal("atendente ChannelScope = nil, want the accessible set")
	}
	if got := *in.ChannelScope; len(got) != 1 || got[0] != chA {
		t.Errorf("atendente ChannelScope = %v, want [%v]", got, chA)
	}
}

// TestChannelScope_AtendenteWithNoAccessSeesEmpty: an atendente with no
// accessible channels gets a non-nil, empty scope — deny-by-default,
// which the read model turns into an empty listing. The chip is not
// rendered for a single-or-zero channel set.
func TestChannelScope_AtendenteWithNoAccessSeesEmpty(t *testing.T) {
	t.Parallel()
	scope := &stubChannelScope{channels: nil}
	summaries := &stubSummaries{}
	h := newHandlerWithChannelScope(t, summaries, scope, false, uuid.New())
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	in := summaries.input()
	if in.ChannelScope == nil {
		t.Fatal("empty-access ChannelScope = nil, want non-nil empty set (deny-by-default)")
	}
	if len(*in.ChannelScope) != 0 {
		t.Errorf("ChannelScope = %v, want empty", *in.ChannelScope)
	}
	if strings.Contains(rec.Body.String(), `name="channel_id"`) {
		t.Error("chip rendered for a zero-channel set; want it hidden")
	}
}

// TestChannelScope_ChipSelectionInScopeIsHonoured: a channel_id within
// the accessible set is forwarded to the read side and marked selected.
func TestChannelScope_ChipSelectionInScopeIsHonoured(t *testing.T) {
	t.Parallel()
	chA, chB := uuid.New(), uuid.New()
	scope := &stubChannelScope{channels: []webinbox.AccessibleChannel{
		{ID: chA, DisplayName: "Suporte"},
		{ID: chB, DisplayName: "Vendas"},
	}}
	summaries := &stubSummaries{}
	h := newHandlerWithChannelScope(t, summaries, scope, true, uuid.New())
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?channel_id="+chB.String(), "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if in := summaries.input(); in.ChannelID != chB {
		t.Errorf("ChannelID = %v, want %v", in.ChannelID, chB)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+chB.String()+`" selected`) {
		t.Errorf("chip did not mark %v selected; body=%q", chB, body)
	}
}

// TestChannelScope_OutOfScopeChipIsDropped: a channel_id NOT among the
// accessible channels is dropped — it never reaches the read side (no
// leak) and does not render a phantom selection.
func TestChannelScope_OutOfScopeChipIsDropped(t *testing.T) {
	t.Parallel()
	chA := uuid.New()
	foreign := uuid.New()
	scope := &stubChannelScope{channels: []webinbox.AccessibleChannel{
		{ID: chA, DisplayName: "Suporte"},
	}}
	summaries := &stubSummaries{}
	h := newHandlerWithChannelScope(t, summaries, scope, false, uuid.New())
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?channel_id="+foreign.String(), "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if in := summaries.input(); in.ChannelID != uuid.Nil {
		t.Errorf("out-of-scope ChannelID leaked to read side: %v", in.ChannelID)
	}
}

// TestChannelScope_UnwiredIsLegacyNoFilter: with no ChannelScope dep the
// read path is unfiltered (scope nil) and the chip is absent — the
// pre-P4 surface is preserved.
func TestChannelScope_UnwiredIsLegacyNoFilter(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if in := summaries.input(); in.ChannelScope != nil {
		t.Errorf("unwired ChannelScope = %v, want nil", *in.ChannelScope)
	}
	if strings.Contains(rec.Body.String(), `name="channel_id"`) {
		t.Error("chip rendered with no ChannelScope dep wired")
	}
}
