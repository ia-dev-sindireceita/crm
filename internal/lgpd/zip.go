package lgpd

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// WriteBundle streams bundle into w as a ZIP archive containing
// data.json (canonical) and data.csv (per-section blocks). It does not
// buffer the whole archive in memory — the underlying zip.Writer pipes
// straight to w. (AC #1 "Streaming, não carregar tudo em RAM".)
//
// The function closes the zip writer but NOT w; the caller owns w.
func WriteBundle(w io.Writer, bundle ExportBundle) error {
	if w == nil {
		return errors.New("lgpd: nil writer")
	}
	zw := zip.NewWriter(w)

	jsonF, err := zw.Create("data.json")
	if err != nil {
		return fmt.Errorf("lgpd: zip create data.json: %w", err)
	}
	enc := json.NewEncoder(jsonF)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bundle); err != nil {
		return fmt.Errorf("lgpd: encode json: %w", err)
	}

	csvF, err := zw.Create("data.csv")
	if err != nil {
		return fmt.Errorf("lgpd: zip create data.csv: %w", err)
	}
	if err := writeCSVSections(csvF, bundle); err != nil {
		return fmt.Errorf("lgpd: write csv: %w", err)
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("lgpd: zip close: %w", err)
	}
	return nil
}

// writeCSVSections emits one CSV section per slice, with a blank line
// and a section banner between them. Each section keeps its own
// header. Spreadsheet importers handle the multi-block layout fine
// (Excel "Get Data → from text" treats blank lines as block separators).
func writeCSVSections(w io.Writer, bundle ExportBundle) error {
	cw := csv.NewWriter(w)

	writeSection := func(banner string, rows [][]string) error {
		if _, err := fmt.Fprintf(w, "# %s\n", banner); err != nil {
			return err
		}
		for _, row := range rows {
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
		return nil
	}

	if err := writeSection("contact", contactRows(bundle.Contact)); err != nil {
		return err
	}
	if err := writeSection("identities", identityRows(bundle.Identities)); err != nil {
		return err
	}
	if err := writeSection("conversations", conversationRows(bundle.Conversations)); err != nil {
		return err
	}
	if err := writeSection("messages", messageRows(bundle.Messages)); err != nil {
		return err
	}
	if err := writeSection("billing_events", billingRows(bundle.BillingEvents)); err != nil {
		return err
	}
	if err := writeSection("consents", consentRows(bundle.Consents)); err != nil {
		return err
	}
	return nil
}

func contactRows(c ExportContact) [][]string {
	return [][]string{
		{"contact_id", "tenant_id", "display_name", "created_at", "updated_at"},
		{
			c.ID.String(),
			c.TenantID.String(),
			c.DisplayName,
			c.CreatedAt.UTC().Format(time.RFC3339),
			c.UpdatedAt.UTC().Format(time.RFC3339),
		},
	}
}

func identityRows(items []ExportIdentity) [][]string {
	out := [][]string{{"identity_id", "channel", "external_id", "created_at"}}
	for _, it := range items {
		out = append(out, []string{
			it.ID.String(),
			it.Channel,
			it.ExternalID,
			it.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func conversationRows(items []ExportConversation) [][]string {
	out := [][]string{{"conversation_id", "channel", "state", "last_message_at", "created_at"}}
	for _, it := range items {
		last := ""
		if it.LastMessageAt != nil {
			last = it.LastMessageAt.UTC().Format(time.RFC3339)
		}
		out = append(out, []string{
			it.ID.String(),
			it.Channel,
			it.State,
			last,
			it.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func messageRows(items []ExportMessage) [][]string {
	out := [][]string{{"message_id", "conversation_id", "direction", "body", "status", "channel_external_id", "media", "created_at"}}
	for _, it := range items {
		external := ""
		if it.ChannelExternalID != nil {
			external = *it.ChannelExternalID
		}
		media := ""
		if it.Media != nil {
			media = *it.Media
		}
		out = append(out, []string{
			it.ID.String(),
			it.ConversationID.String(),
			it.Direction,
			it.Body,
			it.Status,
			external,
			media,
			it.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func billingRows(items []ExportBillingEvent) [][]string {
	out := [][]string{{"event_id", "event_type", "target", "occurred_at"}}
	for _, it := range items {
		out = append(out, []string{
			it.ID.String(),
			it.EventType,
			it.Target,
			it.OccurredAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func consentRows(items []ExportConsent) [][]string {
	out := [][]string{{"consent_id", "scope_kind", "scope_id", "anonymizer_version", "prompt_version", "accepted_at"}}
	for _, it := range items {
		out = append(out, []string{
			it.ID.String(),
			it.ScopeKind,
			it.ScopeID,
			it.AnonymizerVersion,
			it.PromptVersion,
			it.AcceptedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}
