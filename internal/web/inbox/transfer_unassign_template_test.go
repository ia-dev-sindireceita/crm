package inbox

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCustomerPanel_UnassignOption_GatedOnCanUnassign verifies the
// "— Não atribuído —" option (SIN-65480) renders in the Transferir select
// only when the unassign use case is wired (CanUnassign), and that its
// value matches the sentinel the transfer handler dispatches on.
func TestCustomerPanel_UnassignOption_GatedOnCanUnassign(t *testing.T) {
	t.Parallel()
	base := customerPanelData{
		HasConversation: true,
		ConversationID:  uuid.New(),
		Channel:         "whatsapp",
		Assignees:       []AssignableRow{{UserID: uuid.New(), DisplayName: "Ana"}},
	}

	t.Run("wired renders the option", func(t *testing.T) {
		t.Parallel()
		data := base
		data.CanUnassign = true
		var sb strings.Builder
		if err := customerPanelTmpl.Execute(&sb, data); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		out := sb.String()
		if !strings.Contains(out, `value="`+transferUnassignValue+`"`) {
			t.Errorf("missing unassign option value=%q; got %q", transferUnassignValue, out)
		}
		if !strings.Contains(out, "— Não atribuído —") {
			t.Errorf("missing unassign option label; got %q", out)
		}
	})

	t.Run("not wired omits the option", func(t *testing.T) {
		t.Parallel()
		data := base
		data.CanUnassign = false
		var sb strings.Builder
		if err := customerPanelTmpl.Execute(&sb, data); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(sb.String(), "transfer-unassign-option") {
			t.Errorf("unassign option rendered while CanUnassign=false; got %q", sb.String())
		}
	})
}

// TestTransferUnassignValue_NotAUUID pins the sentinel as non-parseable as a
// UUID — the assign path's uuid.Parse must reject it if it ever leaks there,
// so only h.transfer's explicit interception routes it to unassign.
func TestTransferUnassignValue_NotAUUID(t *testing.T) {
	t.Parallel()
	if _, err := uuid.Parse(transferUnassignValue); err == nil {
		t.Fatalf("transferUnassignValue %q parses as a UUID; it must be a non-UUID sentinel", transferUnassignValue)
	}
}
