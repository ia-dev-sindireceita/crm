package inbox

// Render tests for the [SIN-62805] F2-05d media hide flag. The package
// keeps the message_bubble template inside templates.go; these tests
// exercise the template directly via Execute so we do not have to spin
// up the full handler stack to assert on the rendered HTML.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func renderBubble(t *testing.T, msg inboxusecase.MessageView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := messageBubbleTmpl.Execute(&buf, msg); err != nil {
		t.Fatalf("Execute message_bubble: %v", err)
	}
	return buf.String()
}

func baseInbound() inboxusecase.MessageView {
	return inboxusecase.MessageView{
		ID:             uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ConversationID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Direction:      "in",
		Body:           "Hello",
		Status:         "delivered",
		CreatedAt:      time.Now().Add(-1 * time.Minute),
	}
}

func TestMessageBubble_NoMedia_RendersWithoutPlaceholder(t *testing.T) {
	t.Parallel()
	got := renderBubble(t, baseInbound())
	if strings.Contains(got, "message-bubble__media") {
		t.Errorf("nil Media should not render a media block; got: %s", got)
	}
}

func TestMessageBubble_InfectedMedia_RendersBlockedPlaceholder(t *testing.T) {
	t.Parallel()
	msg := baseInbound()
	msg.Media = &inboxusecase.MessageMediaView{ScanStatus: "infected"}
	got := renderBubble(t, msg)

	if !strings.Contains(got, "Conteúdo bloqueado por segurança") {
		t.Errorf("expected security-block placeholder, got: %s", got)
	}
	if !strings.Contains(got, "message-bubble__media--blocked") {
		t.Errorf("expected blocked CSS modifier, got: %s", got)
	}
	if strings.Contains(got, "/t/") {
		t.Errorf("infected bubble must not link to the media route; got: %s", got)
	}
	// The Hash field is the storage key proxy; even if a caller passed
	// it (defensive — the projector should drop it), the template must
	// not render it.
	msg.Media.Hash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	got2 := renderBubble(t, msg)
	if strings.Contains(got2, "deadbeef") {
		t.Errorf("infected bubble leaked the storage hash: %s", got2)
	}
}

func TestMessageBubble_CleanMedia_RendersAttachmentLink(t *testing.T) {
	t.Parallel()
	msg := baseInbound()
	msg.Media = &inboxusecase.MessageMediaView{
		ScanStatus: "clean",
		Hash:       "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
		Format:     "png",
	}
	got := renderBubble(t, msg)
	if !strings.Contains(got, "message-bubble__media--clean") {
		t.Errorf("expected clean CSS modifier, got: %s", got)
	}
	if !strings.Contains(got, "Anexo") {
		t.Errorf("expected Anexo label, got: %s", got)
	}
	if !strings.Contains(got, msg.Media.Hash) {
		t.Errorf("clean bubble must reference the hash, got: %s", got)
	}
	if !strings.Contains(got, `data-format="png"`) {
		t.Errorf("expected data-format attr, got: %s", got)
	}
	if strings.Contains(got, "bloqueado") {
		t.Errorf("clean bubble must not show blocked text, got: %s", got)
	}
}

func TestMessageBubble_PendingMedia_RendersWaitingPlaceholder(t *testing.T) {
	t.Parallel()
	msg := baseInbound()
	msg.Media = &inboxusecase.MessageMediaView{ScanStatus: "pending"}
	got := renderBubble(t, msg)
	if !strings.Contains(got, "Verificando anexo") {
		t.Errorf("expected pending placeholder, got: %s", got)
	}
	if !strings.Contains(got, "message-bubble__media--pending") {
		t.Errorf("expected pending CSS modifier, got: %s", got)
	}
}
