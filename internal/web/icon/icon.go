// Package icon renders a curated subset of Lucide (https://lucide.dev,
// MIT licensed) icons as inline SVG for the Pitho design system.
//
// Icons inherit their color via currentColor and use a 2px stroke with
// round caps/joins, matching the vendored Pitho design handoff
// (design/pitho/flua-design-system/project/components/core/Icon.jsx).
//
// Rendering is inline SVG only — no icon font and no external sprite URL
// — so it is safe under the strict Content-Security-Policy the app ships
// (default-src 'self'; style-src 'self' 'nonce-…' without
// 'unsafe-inline'). For the same reason the emitted <svg> carries NO
// `style` attribute: all sizing and vertical alignment is delegated to
// the `.pitho-icon` class in web/static/css/brand.css. Only presentation
// attributes (width/height/fill/stroke/…), which CSP does not govern, are
// stamped inline.
package icon

import (
	"html/template"
	"sort"
	"strconv"
	"strings"
)

// defaultSize is the rendered px size when a caller omits one. 16px
// matches the handoff component's default and the inline glyphs it
// replaces in app chrome.
const defaultSize = 16

// paths maps an icon name to its inner SVG geometry. The shared frame
// (viewBox="0 0 24 24", 2px stroke, round caps/joins) is applied by
// SVG(); only the shape elements live here.
//
// The entries up to the marker are the verbatim Pitho/Lucide subset
// from the vendored handoff Icon.jsx. Entries after the marker are
// additional Lucide (MIT) glyphs pulled in to cover app chrome the base
// subset did not include.
var paths = map[string]string{
	"search":           `<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>`,
	"plus":             `<path d="M5 12h14"/><path d="M12 5v14"/>`,
	"x":                `<path d="M18 6 6 18"/><path d="m6 6 12 12"/>`,
	"check":            `<path d="M20 6 9 17l-5-5"/>`,
	"chevron-right":    `<path d="m9 18 6-6-6-6"/>`,
	"chevron-left":     `<path d="m15 18-6-6 6-6"/>`,
	"chevron-down":     `<path d="m6 9 6 6 6-6"/>`,
	"chevrons-left":    `<path d="m11 17-5-5 5-5"/><path d="m18 17-5-5 5-5"/>`,
	"more-horizontal":  `<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>`,
	"more-vertical":    `<circle cx="12" cy="12" r="1"/><circle cx="12" cy="5" r="1"/><circle cx="12" cy="19" r="1"/>`,
	"settings":         `<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>`,
	"users":            `<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>`,
	"user":             `<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>`,
	"inbox":            `<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>`,
	"megaphone":        `<path d="m3 11 18-5v12L3 14v-3z"/><path d="M11.6 16.8a3 3 0 1 1-5.8-1.6"/>`,
	"package":          `<path d="M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z"/><path d="M3.3 7 12 12l8.7-5"/><path d="M12 22V12"/>`,
	"credit-card":      `<rect width="20" height="14" x="2" y="5" rx="2"/><path d="M2 10h20"/>`,
	"bar-chart":        `<path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>`,
	"trending-up":      `<path d="M16 7h6v6"/><path d="m22 7-8.5 8.5-5-5L2 17"/>`,
	"zap":              `<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>`,
	"sparkles":         `<path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/>`,
	"bell":             `<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>`,
	"phone":            `<path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/>`,
	"mail":             `<rect width="20" height="16" x="2" y="4" rx="2"/><path d="m22 7-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 7"/>`,
	"message-circle":   `<path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"/>`,
	"calendar":         `<path d="M8 2v4"/><path d="M16 2v4"/><rect width="18" height="18" x="3" y="4" rx="2"/><path d="M3 10h18"/>`,
	"clock":            `<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>`,
	"panel-left":       `<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>`,
	"layout-dashboard": `<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>`,
	"git-branch":       `<line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>`,
	"filter":           `<polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3"/>`,
	"trash":            `<path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>`,
	"edit":             `<path d="M12 20h9"/><path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z"/>`,
	"building":         `<rect width="16" height="20" x="4" y="2" rx="2"/><path d="M9 22v-4h6v4"/><path d="M8 6h.01"/><path d="M16 6h.01"/><path d="M12 6h.01"/><path d="M12 10h.01"/><path d="M12 14h.01"/><path d="M16 10h.01"/><path d="M16 14h.01"/><path d="M8 10h.01"/><path d="M8 14h.01"/>`,
	"dollar-sign":      `<line x1="12" x2="12" y1="2" y2="22"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/>`,
	"arrow-up-right":   `<path d="M7 7h10v10"/><path d="M7 17 17 7"/>`,
	"star":             `<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>`,
	"circle":           `<circle cx="12" cy="12" r="10"/>`,
	"check-circle":     `<path d="M21.801 10A10 10 0 1 1 17 3.335"/><path d="m9 11 3 3L22 4"/>`,
	"sun":              `<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>`,
	"moon":             `<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>`,

	// --- additional Lucide (MIT) glyphs for app chrome ---
	// octagon-alert: replaces the 🛑 stop emoji in the master
	// impersonation banner — an octagonal "halt" sign with an alert bar.
	"octagon-alert": `<path d="M12 16h.01"/><path d="M12 8v4"/><path d="M15.312 2a2 2 0 0 1 1.414.586l4.688 4.688A2 2 0 0 1 22 8.688v6.624a2 2 0 0 1-.586 1.414l-4.688 4.688a2 2 0 0 1-1.414.586H8.688a2 2 0 0 1-1.414-.586l-4.688-4.688A2 2 0 0 1 2 15.312V8.688a2 2 0 0 1 .586-1.414l4.688-4.688A2 2 0 0 1 8.688 2z"/>`,
	// lock: replaces the 🔒 padlock emoji on the master grant-request
	// "SOLICITANTE" sigil — a closed padlock framing the 4-eyes column.
	"lock": `<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>`,
	// paperclip: replaces the 📎 attachment emoji on the inbox
	// clean-media message bubble (SIN-65118).
	"paperclip": `<path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/>`,
	// check-check: replaces the ✓✓ double-check delivery glyph on the
	// inbox outbound status badge (delivered/read) — SIN-65118.
	"check-check": `<path d="M18 6 7 17l-5-5"/><path d="m22 10-7.5 7.5L13 16"/>`,
	// triangle-alert: replaces the ⚠ warning glyph on the inbox
	// outbound status badge (failed delivery) — SIN-65118.
	"triangle-alert": `<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>`,
}

// filledIcons lists icons rendered as a solid fill rather than a stroke
// (mirrors the `filled` branch of the handoff Icon component).
var filledIcons = map[string]bool{"circle": true}

// SVG renders the named icon as inline SVG at the given pixel size.
// A non-positive size falls back to the default. Unknown names render
// the empty string — fail-safe, never panics and never emits a broken
// tag, mirroring the handoff component's null return for missing names.
func SVG(name string, size int) template.HTML {
	inner, ok := paths[name]
	if !ok {
		return ""
	}
	if size <= 0 {
		size = defaultSize
	}
	dim := strconv.Itoa(size)

	var b strings.Builder
	b.WriteString(`<svg class="pitho-icon" width="`)
	b.WriteString(dim)
	b.WriteString(`" height="`)
	b.WriteString(dim)
	b.WriteString(`" viewBox="0 0 24 24" `)
	if filledIcons[name] {
		b.WriteString(`fill="currentColor" stroke="none"`)
	} else {
		b.WriteString(`fill="none" stroke="currentColor"`)
	}
	b.WriteString(` stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`)
	b.WriteString(inner)
	b.WriteString(`</svg>`)
	// The geometry is a compile-time constant from this package and the
	// name is matched against a fixed allow-list, so the result is safe
	// to emit unescaped.
	return template.HTML(b.String()) //nolint:gosec // constant, allow-listed SVG markup
}

// Has reports whether name is a known icon.
func Has(name string) bool {
	_, ok := paths[name]
	return ok
}

// Names returns the sorted list of available icon names. Useful for
// tests and design audits.
func Names() []string {
	out := make([]string, 0, len(paths))
	for n := range paths {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// FuncMap exposes the icon helper for html/template:
//
//	{{icon "search"}}      renders at the default 16px size
//	{{icon "search" 20}}   renders at 20px
//
// Only the first variadic size is honored; extras are ignored.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"icon": func(name string, size ...int) template.HTML {
			s := defaultSize
			if len(size) > 0 {
				s = size[0]
			}
			return SVG(name, s)
		},
	}
}
