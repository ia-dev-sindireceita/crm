package inter

import (
	"bytes"
	"encoding/base64"
	"image/png"

	"rsc.io/qr"
)

// qrEncodeBase64 renders the PIX EMVCo "copia-e-cola" payload to a
// PNG QR code and returns the base64 of that PNG. The output matches
// the contract documented on pix.ChargeResponse.QRCode: a base64
// payload the UI layer wraps in a data:image/png;base64,… URI.
//
// Why rsc.io/qr: it is a pure-Go, zero-transitive-dep, single-author
// (Russ Cox) encoder. The whole library is ~1.5k LOC and has been
// stable since 2017. Adding the dep is part of [SIN-62958]; the PR
// description spells out the rationale for CTO sign-off, per the
// "New dependencies require CTO sign-off" rule in CLAUDE.md.
//
// Error level Q (≈25% codeword recovery) matches BACEN's
// recommendation for printed PIX BR Codes — high enough that a smudge
// on a paper invoice still scans, low enough to keep the bitmap from
// growing past ~1 KB for a typical 200-char EMVCo payload.
//
// The caller (Charger.Create) guards against an empty payload before
// reaching here, so we can lean on rsc.io/qr's own error surface.
func qrEncodeBase64(payload string) (string, error) {
	code, err := qr.Encode(payload, qr.Q)
	if err != nil {
		return "", err
	}
	img := code.Image()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
