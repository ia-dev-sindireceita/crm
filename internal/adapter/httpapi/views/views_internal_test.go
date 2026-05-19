package views

import (
	"html/template"
	"testing"
)

// TestTenantThemeStyle_TableDriven covers every branch of the
// FuncMap helper. Each case maps to a data shape the layout might
// see — including legacy structs that don't declare the field and the
// reflective edge cases (nil interface, nil pointer, wrong type).
func TestTenantThemeStyle_TableDriven(t *testing.T) {
	t.Parallel()

	type withField struct {
		TenantThemeStyle template.CSS
	}
	type withStringField struct {
		Whatever         string
		TenantThemeStyle string
	}
	type noField struct {
		Other string
	}
	type otherType struct {
		TenantThemeStyle int
	}

	style := template.CSS(":root{--color-primary:#abcdef}")

	cases := []struct {
		name string
		in   any
		want template.CSS
	}{
		{"nil", nil, ""},
		{"struct with template.CSS field", withField{TenantThemeStyle: style}, style},
		{"pointer to struct", &withField{TenantThemeStyle: style}, style},
		{"struct with string field", withStringField{TenantThemeStyle: string(style)}, style},
		{"struct without field", noField{Other: "x"}, ""},
		{"struct with wrong field type", otherType{TenantThemeStyle: 42}, ""},
		{"non-struct", "just a string", ""},
		{"nil pointer", (*withField)(nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tenantThemeStyle(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTenantThemeStyle_InterfaceWrapper covers the reflect.Interface
// kind branch — the helper unwraps interfaces just like pointers so a
// handler that passes `any(data)` through chi's render plumbing still
// gets the right field.
func TestTenantThemeStyle_InterfaceWrapper(t *testing.T) {
	t.Parallel()
	type d struct{ TenantThemeStyle template.CSS }
	var iface any = d{TenantThemeStyle: ":root{--color-primary:#000}"}
	got := tenantThemeStyle(iface)
	if got != template.CSS(":root{--color-primary:#000}") {
		t.Fatalf("interface unwrap failed: %q", got)
	}
}
