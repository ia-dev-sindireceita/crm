package wasession

import (
	"html/template"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// qrSVG renders a WhatsApp Web pairing payload as an inline SVG QR code.
//
// Why inline SVG (and never <img src="data:…"> or an external QR service),
// same rationale as internal/adapter/httpapi/usermfa/qr.go:
//
//   - Secret containment: the pairing payload is a bearer secret — anyone
//     who can read it can hijack the pairing. Generating the QR server-side
//     and emitting it inline keeps the secret on the tenant's origin; we
//     never build an <img src> pointing at a third-party QR service (SSRF /
//     secret leak, OWASP A02).
//   - CSP: the app ships `default-src 'self'` with no `img-src`, so a
//     `data:` image is blocked. An inline <svg> is document markup, not a
//     fetched resource, so it needs no CSP relaxation.
//
// The SVG is built purely from the boolean module matrix and integer
// coordinates — no part of the payload is interpolated into the markup — so
// the template.HTML result carries no untrusted markup.
func qrSVG(payload string) (template.HTML, error) {
	code, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bitmap := code.Bitmap()
	n := len(bitmap)

	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(n))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" width="240" height="240" shape-rendering="crispEdges" role="img" `)
	b.WriteString(`aria-label="QR code para parear a sessão do WhatsApp" class="wa-session__qr-svg">`)
	// White backing keeps the QR scannable on dark themes.
	b.WriteString(`<rect width="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" fill="#ffffff"/>`)
	// One rect per horizontal run of dark modules keeps the markup compact.
	for y, row := range bitmap {
		x := 0
		for x < len(row) {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			b.WriteString(`<rect x="`)
			b.WriteString(strconv.Itoa(start))
			b.WriteString(`" y="`)
			b.WriteString(strconv.Itoa(y))
			b.WriteString(`" width="`)
			b.WriteString(strconv.Itoa(x - start))
			b.WriteString(`" height="1" fill="#000000"/>`)
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), nil
}
