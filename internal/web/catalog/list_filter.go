package catalog

import (
	"net/http"
	"sort"
	"strings"

	"github.com/pericles-luz/crm/internal/catalog"
)

// UncategorizedKey is the synthetic value the sidebar uses when the
// operator wants to filter the list to products that have no category
// set. Empty string would round-trip ambiguously through the URL — the
// "filter not set" and "filter explicitly empty" cases collapse —
// so the sidebar emits this explicit sentinel.
const UncategorizedKey = "__uncategorized__"

// listFilter is the parsed query-string filter the catalog list page
// applies on top of the per-tenant product list. Both fields are
// optional; the zero value renders the unfiltered list.
type listFilter struct {
	// Query is the case-insensitive substring match applied to
	// product name (?q=). Tagged "Search" in the view layer for
	// operator-facing copy.
	Query string

	// Category narrows the list to products whose category matches
	// exactly. UncategorizedKey selects products with no category.
	Category string
}

// parseListFilter pulls the supported filter knobs out of the request
// URL. Unknown query keys are ignored; values are trimmed so a stray
// trailing space in the URL does not silently break exact match.
func parseListFilter(r *http.Request) listFilter {
	if r == nil || r.URL == nil {
		return listFilter{}
	}
	q := r.URL.Query()
	return listFilter{
		Query:    strings.TrimSpace(q.Get("q")),
		Category: strings.TrimSpace(q.Get("category")),
	}
}

// applyListFilter returns the products that survive the filter. The
// input is left untouched — callers can pass a shared slice without
// risk of accidental mutation. An empty filter returns a copy of the
// input so callers do not have to special-case the zero value.
func applyListFilter(in []*catalog.Product, f listFilter) []*catalog.Product {
	if len(in) == 0 {
		return nil
	}
	needle := strings.ToLower(f.Query)
	out := make([]*catalog.Product, 0, len(in))
	for _, p := range in {
		if p == nil {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(p.Name()), needle) {
			continue
		}
		switch f.Category {
		case "":
			// no category filter
		case UncategorizedKey:
			if p.Category() != "" {
				continue
			}
		default:
			if p.Category() != f.Category {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// CategoryBucket is one entry in the catalog sidebar — a category name
// (or the sentinel UncategorizedKey) plus the product count. The
// sidebar renders the bucket as a filter link.
type CategoryBucket struct {
	// Key is the value the link emits in ?category=. Always
	// non-empty: UncategorizedKey for products with no category.
	Key string

	// Label is the operator-facing copy. Matches Key for real
	// categories; "Sem categoria" for UncategorizedKey.
	Label string

	// Count is the number of products under this bucket. Includes
	// every tenant product (the search filter does not change the
	// sidebar so the operator can always navigate back).
	Count int
}

// buildCategoryCounts groups the supplied products by category and
// returns the sidebar buckets. Buckets are sorted alphabetically with
// the "Sem categoria" bucket pinned last so it does not jostle as the
// catalog grows. An empty product list returns nil so the template's
// {{range}} guard renders the empty-state copy.
func buildCategoryCounts(in []*catalog.Product) []CategoryBucket {
	if len(in) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, p := range in {
		if p == nil {
			continue
		}
		c := p.Category()
		if c == "" {
			counts[UncategorizedKey]++
			continue
		}
		counts[c]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]CategoryBucket, 0, len(counts))
	for k, v := range counts {
		label := k
		if k == UncategorizedKey {
			label = "Sem categoria"
		}
		out = append(out, CategoryBucket{Key: k, Label: label, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		// pin uncategorized last
		if out[i].Key == UncategorizedKey {
			return false
		}
		if out[j].Key == UncategorizedKey {
			return true
		}
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	return out
}
