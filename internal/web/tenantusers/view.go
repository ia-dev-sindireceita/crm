package tenantusers

import (
	"html/template"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// roleOption is one selectable role in the create / edit form. Key is the
// stored users.role value; Label is the operator-facing pt-BR name. The set
// is closed to the two assignable roles (anti-escalation): master and the
// non-assignable tenant roles never appear in the picker.
type roleOption struct {
	Key   string
	Label string
}

// assignableRoles is the ordered, closed set the surface offers. It mirrors
// tenantusers.RoleAssignable so the picker and the domain guard never
// disagree.
var assignableRoles = []roleOption{
	{Key: string(iam.RoleTenantGerente), Label: "Gerente"},
	{Key: string(iam.RoleTenantAtendente), Label: "Atendente"},
}

// roleOptions returns a fresh copy so callers never mutate the package set.
func roleOptions() []roleOption {
	return append([]roleOption(nil), assignableRoles...)
}

// roleLabel maps a stored role string to its operator label, falling back
// to the raw value so a legacy/unexpected role never renders blank.
func roleLabel(role string) string {
	switch role {
	case string(iam.RoleTenantGerente):
		return "Gerente"
	case string(iam.RoleTenantAtendente):
		return "Atendente"
	case string(iam.RoleTenantCommon):
		return "Comum"
	case string(iam.RoleTenantLider):
		return "Líder"
	case "admin":
		return "Admin (MFA)"
	default:
		return role
	}
}

// userRow is one row in the users table.
type userRow struct {
	ID    string
	Email string
	// RoleKey is the stored role (drives the edit form's <select>).
	RoleKey string
	// RoleLabel is the operator-facing role name.
	RoleLabel string
	Active    bool
	// MustChangePassword flags a user that still holds the temporary
	// password (renders a "senha temporária" pill so the gerente knows the
	// account has not completed first-login rotation yet).
	MustChangePassword bool
}

func rowFromUser(u *tenantusers.User) userRow {
	return userRow{
		ID:                 u.ID.String(),
		Email:              u.Email,
		RoleKey:            string(u.Role),
		RoleLabel:          roleLabel(string(u.Role)),
		Active:             u.Active,
		MustChangePassword: u.MustChangePassword,
	}
}

// pageData is the full /settings/users page view model. It embeds the shell
// chrome fields (read by the shell layout via reflection) so the surface
// renders inside the shared SidebarNav app-shell.
type pageData struct {
	Rows []userRow

	TenantName       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
}

// modalData drives the create / edit-role form rendered into
// #tenantusers-modal.
type modalData struct {
	IsNew   bool
	Action  string // POST target
	ID      string
	Email   string
	RoleKey string // selected role key
	Roles   []roleOption
	// FieldError names the field that failed validation ("email", "role")
	// so the template renders the inline error next to it; empty = none.
	FieldError string
	// ErrorMessage is the human-facing error text.
	ErrorMessage string
}

// credentialData drives the one-time temporary-password card shown after a
// successful create. It is rendered once into the modal and never persisted
// client-side; a reload loses it (by design — the plaintext lives only in
// this response).
type credentialData struct {
	Email        string
	TempPassword string
}

// listRefresh drives the OOB response after a create / role change /
// deactivate: the whole table is re-rendered plus a success toast.
type listRefresh struct {
	Rows  []userRow
	Toast toastData
}

// createRefresh drives the OOB response after a successful create: the
// one-time credential card (primary target #tenantusers-modal) plus the
// OOB list refresh and toast.
type createRefresh struct {
	Credential credentialData
	Rows       []userRow
	Toast      toastData
}

// toastData drives the OOB success toast.
type toastData struct {
	Message string
}

// parseRole maps a submitted role key to an iam.Role, reporting whether it
// is an assignable value. A forged/escalated key (master, tenant_common,
// junk) returns ok=false so the handler bounces the form.
func parseRole(key string) (iam.Role, bool) {
	r := iam.Role(key)
	return r, tenantusers.RoleAssignable(r)
}
