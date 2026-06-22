package usermfa

import (
	"html/template"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// otpauthQRCodeSVG renders an otpauth:// URI as an inline SVG QR code that
// authenticator apps can scan.
//
// Why inline SVG and not <img src="data:…"> or an external QR service:
//
//   - SSRF / secret leakage (OWASP A02): the otpauth URI embeds the TOTP
//     secret. The QR is therefore generated server-side and emitted inline
//     so the secret never leaves the tenant's origin. We NEVER build an
//     <img src> pointing at api.qrserver.com / chart.googleapis.com / etc.
//   - CSP: the app ships `default-src 'self'` with no `img-src`, so images
//     fall back to 'self' and a `data:` image would be blocked. An inline
//     <svg> is document markup, not a fetched resource, so it needs neither
//     an `img-src 'self' data:` relaxation nor a per-route CSP override.
//
// The SVG is built purely from the boolean module matrix and integer
// coordinates — no part of otpauthURI is interpolated into the markup — so
// the template.HTML result carries no untrusted markup. The skip2/go-qrcode
// bitmap already includes the 4-module quiet zone required for reliable
// scanning.
func otpauthQRCodeSVG(otpauthURI string) (template.HTML, error) {
	code, err := qrcode.New(otpauthURI, qrcode.Medium)
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
	b.WriteString(`" width="220" height="220" shape-rendering="crispEdges" role="img" `)
	b.WriteString(`aria-label="QR code para configurar a verificação em duas etapas no app autenticador" class="totp-qr__svg">`)
	// White backing so the QR stays scannable on dark themes.
	b.WriteString(`<rect width="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" fill="#ffffff"/>`)
	// Emit one rect per horizontal run of dark modules to keep the markup
	// compact (~half the rects of a per-module emit).
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
