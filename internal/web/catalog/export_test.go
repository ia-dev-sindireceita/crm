package catalog

import (
	"net/http"

	domain "github.com/pericles-luz/crm/internal/catalog"
)

// Test-only re-exports. Keeps the production surface tight while
// letting catalog_test exercise the unexported list-filter helpers
// without an in-package _internal_test.go split.

type ExportedListFilter = listFilter

func ExportedParseListFilter(r *http.Request) ExportedListFilter {
	return parseListFilter(r)
}

func ExportedApplyListFilter(in []*domain.Product, f ExportedListFilter) []*domain.Product {
	return applyListFilter(in, f)
}

func ExportedBuildCategoryCounts(in []*domain.Product) []CategoryBucket {
	return buildCategoryCounts(in)
}
