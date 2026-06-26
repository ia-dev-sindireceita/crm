package master

import (
	"html/template"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/shell"
)

// Test-only exports so the external _test package can drive small
// pure helpers without making them part of the public API.

// ExportEnsureGrantPresent surfaces ensureGrantPresent for tests.
func ExportEnsureGrantPresent(grants []GrantRow, grant GrantRow) []GrantRow {
	return ensureGrantPresent(grants, grant)
}

// ExportInt64ToStr surfaces int64ToStr for tests.
func ExportInt64ToStr(n int64) string { return int64ToStr(n) }

// ExportFormatGrantTime surfaces formatGrantTime for tests.
func ExportFormatGrantTime(t time.Time) string { return formatGrantTime(t) }

// ExportGrantKindLabel surfaces grantKindLabel for tests.
func ExportGrantKindLabel(k GrantKind) string { return grantKindLabel(k) }

// ExportReadInt64Payload surfaces readInt64Payload for tests.
func ExportReadInt64Payload(p map[string]any, key string) int64 {
	return readInt64Payload(p, key)
}

// ExportMasterLayoutTmpl surfaces the tenants list layout template for
// banner-rendering tests.
func ExportMasterLayoutTmpl() *template.Template { return masterLayoutTmpl }

// ExportTenantDetailLayoutTmpl surfaces the tenant-detail layout for
// banner + impersonate-trigger tests.
func ExportTenantDetailLayoutTmpl() *template.Template { return tenantDetailLayoutTmpl }

// ExportGrantRequestDetailPanelTmpl surfaces the panel template for
// 4-eyes confirm-twice + self-approve guard tests.
func ExportGrantRequestDetailPanelTmpl() *template.Template { return grantRequestDetailPanelTmpl }

// ExportNewTenantsListData builds a tenants list pageData with the
// given rows + active impersonation context, for banner tests.
func ExportNewTenantsListData(rows []TenantRow, banner *shell.ImpersonationContext) interface{} {
	return pageData{
		Tenants:             rows,
		Page:                1,
		PageSize:            25,
		TotalPages:          1,
		ActiveImpersonation: banner,
	}
}

// ExportNewTenantDetailData builds a tenant-detail pageData for tests.
func ExportNewTenantDetailData(row TenantRow, showReasonModal bool, banner *shell.ImpersonationContext) interface{} {
	return tenantDetailData{
		Tenant:              row,
		ShowReasonModal:     showReasonModal,
		ActiveImpersonation: banner,
	}
}

// ExportNewGrantRequestDetailData builds a grant-request detail
// pageData for self-approve + confirm-twice tests.
func ExportNewGrantRequestDetailData(req GrantRequest, viewer uuid.UUID, stage string, banner *shell.ImpersonationContext) interface{} {
	return grantRequestDetailData{
		Request:             req,
		CurrentUserID:       viewer,
		ConfirmStage:        stage,
		ActiveImpersonation: banner,
	}
}

// ExportFormatImpersonationISO surfaces formatImpersonationISO for
// banner helpers tests.
func ExportFormatImpersonationISO(t time.Time) string { return formatImpersonationISO(t) }

// ExportTruncateImpersonationReason surfaces truncateImpersonationReason
// for banner helpers tests.
func ExportTruncateImpersonationReason(s string) string { return truncateImpersonationReason(s) }

// ExportLedgerSourceLabel surfaces ledgerSourceLabel for table-driven
// label tests (SIN-63987 coverage).
func ExportLedgerSourceLabel(s string) string { return ledgerSourceLabel(s) }

// ExportLedgerKindLabel surfaces ledgerKindLabel for table-driven label
// tests (SIN-63987 coverage).
func ExportLedgerKindLabel(k string) string { return ledgerKindLabel(k) }

// ExportIsZeroUUID surfaces isZeroUUID for the template "no link target"
// branch test (SIN-63987 coverage).
func ExportIsZeroUUID(id uuid.UUID) bool { return isZeroUUID(id) }
