package channels

import (
	"html/template"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// channelType is one selectable channel family in the create form. Key
// is the stored channel_key (lower-case, the domain's addressing family)
// and Label is the operator-facing name. The list is closed: the create
// handler rejects any channel_key not in this set so the picker and the
// storage layer never disagree.
type channelType struct {
	Key   string
	Label string
}

// Channel-key constants for the WhatsApp families. The legacy key stays
// "whatsapp" (relabelled "WhatsApp API") so existing channels need zero
// data migration; "whatsapp_web" is a distinct stored value for the
// unofficial QR-session family (SIN-66459).
const (
	channelKeyWhatsApp    = "whatsapp"
	channelKeyWhatsAppWeb = "whatsapp_web"
)

// channelTypes is the ordered, closed set of channel families the admin
// surface knows about. Order drives the <select> option order. It is the
// full registry used for label lookup; the create form is served a
// flag-filtered view via channelTypesFor so a gated family never appears
// unless its flag is on.
var channelTypes = []channelType{
	{Key: channelKeyWhatsApp, Label: "WhatsApp API"},
	{Key: channelKeyWhatsAppWeb, Label: "WhatsApp Web"},
	{Key: "telegram", Label: "Telegram"},
	{Key: "instagram", Label: "Instagram"},
	{Key: "webchat", Label: "Webchat"},
	{Key: "email", Label: "E-mail"},
}

// channelTypesFor returns the create-form type list filtered to the
// operator's enabled feature flags. whatsapp_web is only offered when
// whatsappWebEnabled is true (flag default OFF, prod-safe); every other
// family is always offered. The returned slice is a fresh copy so callers
// never mutate the package registry.
func channelTypesFor(whatsappWebEnabled bool) []channelType {
	out := make([]channelType, 0, len(channelTypes))
	for _, t := range channelTypes {
		if t.Key == channelKeyWhatsAppWeb && !whatsappWebEnabled {
			continue
		}
		out = append(out, t)
	}
	return out
}

// typeLabel maps a stored channel_key to its operator-facing label,
// falling back to the raw key (title-cased) for a legacy/backfilled key
// outside the closed set so the registry never renders a blank type.
func typeLabel(key string) string {
	for _, t := range channelTypes {
		if t.Key == key {
			return t.Label
		}
	}
	if key == "" {
		return "—"
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

// validTypeFor reports whether key is an offered channel family given the
// operator's enabled flags. It shares channelTypesFor's filter so the
// picker and the create-time guard can never disagree: with the whatsapp_web
// flag OFF a forged POST for that key is rejected (deny-by-default,
// secure-by-default API), and it is accepted only once the flag is on.
func validTypeFor(key string, whatsappWebEnabled bool) bool {
	for _, t := range channelTypesFor(whatsappWebEnabled) {
		if t.Key == key {
			return true
		}
	}
	return false
}

// roleLabel maps a raw tenant role string to a short operator label for
// the roster row. Unknown roles fall back to the raw value so the roster
// never renders a blank secondary label.
func roleLabel(role string) string {
	switch role {
	case "tenant_gerente":
		return "Gerente"
	case "tenant_atendente":
		return "Atendente"
	case "tenant_lider":
		return "Líder"
	case "tenant_common":
		return "Comum"
	default:
		return role
	}
}

// maskIdentity renders a channel's external identity with the middle
// digits/characters masked (LGPD data-minimisation, spec D4). It keeps a
// leading "+" or "@" and the last two characters visible and replaces the
// rest with the middle-dot "·". An empty identity renders as "—" so the
// cell is never blank. This is presentation-only; the full identity is
// still stored and only ever rendered masked in operator views.
func maskIdentity(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	prefix := ""
	if s[0] == '+' || s[0] == '@' {
		prefix = s[:1]
		s = s[1:]
	}
	// Strip non-alphanumerics for the masking count but preserve the tail.
	runes := []rune(s)
	if len(runes) <= 2 {
		return prefix + strings.Repeat("·", len(runes))
	}
	tail := string(runes[len(runes)-2:])
	return prefix + strings.Repeat("·", len(runes)-2) + tail
}

// channelRow is one row in the registry table.
type channelRow struct {
	ID             string
	Name           string
	TypeLabel      string
	MaskedIdentity string
	Active         bool
	// AccessSummary is the operator label for who can attend the channel:
	// "Todos" (every roster user granted) or "N atendentes".
	AccessSummary string
	// AccessAll is true when every roster user is granted (renders the
	// neutral "Todos" badge); false renders the informational "N
	// atendentes" badge.
	AccessAll bool
	// Restricted mirrors the channel's restricted flag: true renders the
	// "Restrito" access-mode badge (membership is enforced), false renders
	// "Aberto" (every atendente of the tenant sees the channel). It is the
	// visible signal that the roster below is actually enforced.
	Restricted bool
}

// rosterEntry is one checkbox row in the shared access-roster primitive.
type rosterEntry struct {
	ID          string
	DisplayName string
	RoleLabel   string
	Checked     bool
}

// rosterView bundles the roster entries with the live count line data so
// the fieldset partial renders "N de M com acesso" without recomputing.
type rosterView struct {
	Entries []rosterEntry
	Checked int
	Total   int
}

// buildRoster maps the tenant roster users into checkbox entries,
// pre-checking those whose id is in granted. granted==nil with allChecked
// true pre-checks everyone (the new-channel default, spec D2/D3).
func buildRoster(users []channels.RosterUser, granted map[uuid.UUID]struct{}, allChecked bool) rosterView {
	entries := make([]rosterEntry, 0, len(users))
	checked := 0
	for _, u := range users {
		isChecked := allChecked
		if granted != nil {
			_, isChecked = granted[u.ID]
		}
		if isChecked {
			checked++
		}
		entries = append(entries, rosterEntry{
			ID:          u.ID.String(),
			DisplayName: u.DisplayName,
			RoleLabel:   roleLabel(u.Role),
			Checked:     isChecked,
		})
	}
	return rosterView{Entries: entries, Checked: checked, Total: len(users)}
}

// accessSummary derives the registry access chip from the grant count and
// the roster total. A channel granted to every roster user (and at least
// one exists) reads "Todos"; otherwise "N atendentes". A zero-roster
// tenant reads "0 atendentes" (never "Todos", which would be misleading).
func accessSummary(grantCount, rosterTotal int) (label string, all bool) {
	if rosterTotal > 0 && grantCount >= rosterTotal {
		return "Todos", true
	}
	return pluralAtendentes(grantCount), false
}

func pluralAtendentes(n int) string {
	if n == 1 {
		return "1 atendente"
	}
	return itoa(n) + " atendentes"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// sortRosterByLabel keeps the roster order stable + human (by display
// label, then id) independent of the adapter's ordering, so the create
// and edit forms read identically.
func sortRosterByLabel(users []channels.RosterUser) {
	sort.SliceStable(users, func(i, j int) bool {
		if users[i].DisplayName == users[j].DisplayName {
			return users[i].ID.String() < users[j].ID.String()
		}
		return users[i].DisplayName < users[j].DisplayName
	})
}

// pageData is the full /settings/channels registry page view model. It
// embeds the shell chrome fields (read by the shell layout via reflection)
// so the surface renders inside the shared SidebarNav app-shell.
type pageData struct {
	Rows []channelRow

	TenantName       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
}

// modalData drives the create / edit form rendered into #channels-modal.
type modalData struct {
	IsNew      bool
	Action     string // POST target
	ID         string
	Name       string
	ChannelKey string // selected type key
	Identity   string
	Types      []channelType
	Roster     rosterView
	// Restricted drives the "Acesso restrito" checkbox state. On the new
	// form it defaults false (open, zero-regression); on edit it mirrors
	// the stored flag; on a validation bounce it echoes the submitted
	// value so the operator's toggle is never silently lost.
	Restricted bool
	// FieldError names the field that failed validation ("name",
	// "identity", "type") so the template renders the inline error next to
	// it; empty means no field-level error.
	FieldError string
	// ErrorMessage is the human-facing error text shown in the field error
	// and/or the summary alert.
	ErrorMessage string
}

// toastData drives the OOB success toast.
type toastData struct {
	Message string
}
