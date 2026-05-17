package master

import "time"

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
