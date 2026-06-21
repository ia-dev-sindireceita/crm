package inbox

// LeadReason names the reason a leadership change was recorded in
// assignment_history. The set MUST match migration 0092's CHECK
// constraint on `assignment_history.reason`; the database is the source
// of truth and Valid() is the in-memory guard so callers get a clean
// error before hitting a constraint violation.
type LeadReason string

const (
	// LeadReasonLead — automatic attribution from
	// tenant.default_lead_user_id at conversation creation.
	LeadReasonLead LeadReason = "lead"
	// LeadReasonManual — operator selected the new assignee through
	// the inbox UI.
	LeadReasonManual LeadReason = "manual"
	// LeadReasonReassign — supervisor/admin handed the conversation
	// off to a different operator.
	LeadReasonReassign LeadReason = "reassign"
	// LeadReasonUnassign — the conversation was returned to the Não
	// atribuído state (SIN-65480). The matching assignment_history row
	// names no user (user_id IS NULL); migration 0124 widened the reason
	// CHECK and added the user_id presence CHECK to make that row legal.
	LeadReasonUnassign LeadReason = "unassign"
)

// Valid reports whether r is one of the four accepted reason values. The
// CHECK constraint in migration 0092 (widened by 0124 to add 'unassign')
// rejects anything else; this guard catches programmer errors at the
// domain boundary.
func (r LeadReason) Valid() bool {
	switch r {
	case LeadReasonLead, LeadReasonManual, LeadReasonReassign, LeadReasonUnassign:
		return true
	}
	return false
}
