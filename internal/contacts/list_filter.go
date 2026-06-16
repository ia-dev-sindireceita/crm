package contacts

import "strings"

// DefaultListLimit caps a contacts list page when the caller passes a
// zero or negative limit. The HTMX list pane renders this many rows by
// default; the use-case layer applies it so handlers do not hard-code
// pagination policy.
const DefaultListLimit = 50

// MaxListLimit is the hard upper bound on a single page. A caller asking
// for more is clamped down so a hostile or buggy client cannot ask the
// adapter to materialise an unbounded result set.
const MaxListLimit = 200

// ListFilter is the domain value object describing a contacts list/search
// query. It is intentionally storage- and transport-agnostic: no SQL or
// HTTP types leak into the core (Hexagonal lens). The adapter turns Query
// into a parameterised ILIKE over the contact display name plus the
// linked channel identities (phone/email); Limit/Offset drive
// deterministic pagination.
//
// Construct freely and call Normalized before handing it to the
// Repository so the limit/offset clamps and whitespace trimming apply
// uniformly regardless of who built the filter.
type ListFilter struct {
	// Query is the free-text search term. Empty means "no text filter"
	// (return every contact under the tenant scope, paginated). It is
	// matched case-insensitively against the contact name and the
	// external ids of the contact's channel identities.
	Query string
	// Limit caps the page size. A zero or negative value resolves to
	// DefaultListLimit via Normalized; anything above MaxListLimit is
	// clamped down.
	Limit int
	// Offset is the number of rows to skip for offset pagination. A
	// negative value resolves to 0 via Normalized.
	Offset int
}

// Normalized returns a copy of f with Query trimmed of surrounding
// whitespace, Limit defaulted/clamped into (0, MaxListLimit], and Offset
// floored at zero. It never mutates the receiver, so callers can keep the
// raw filter for echoing back to the UI while passing the normalised one
// to storage.
func (f ListFilter) Normalized() ListFilter {
	out := ListFilter{
		Query:  strings.TrimSpace(f.Query),
		Limit:  f.Limit,
		Offset: f.Offset,
	}
	if out.Limit <= 0 {
		out.Limit = DefaultListLimit
	}
	if out.Limit > MaxListLimit {
		out.Limit = MaxListLimit
	}
	if out.Offset < 0 {
		out.Offset = 0
	}
	return out
}
