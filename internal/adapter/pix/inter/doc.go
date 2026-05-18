// Package inter implements pix.PIXCharger on top of the Banco Inter PIX
// API (https://developers.inter.co), as ratified by board decision D2
// in [SIN-62205] and the AC of [SIN-62958].
//
// Wire shape: mTLS at the TCP boundary + OAuth2 client_credentials at
// the application boundary. Both layers are independent — mTLS proves
// "this Inter Banking client", the bearer proves "scoped to PIX cob.*
// / pix.read for this client". Inter rejects either half on its own.
//
// Endpoints used:
//
//   - POST /oauth/v2/token            — token issuance, form-encoded.
//   - PUT  /cob/{txid}                — create immediate charge.
//   - GET  /cob/{txid}                — read charge status.
//
// Out of scope here:
//
//   - The PIX recipient ("chave") and the merchant CPF/CNPJ. Both are
//     SaaS-global and live in Config — they never enter the
//     pix.ChargeRequest payload.
//   - QR image rendering. The Inter API only returns pixCopiaECola
//     (the EMVCo "copia-e-cola" string). The adapter renders the
//     payload to a PNG locally via rsc.io/qr so the UI layer can
//     serve it as a data URI; see qr.go for the rationale.
//
// Security posture (AC#2, AC#6):
//
//   - Client_id / client_secret / cert / key paths come from env, never
//     from a file in the repo and never from a DB row.
//   - The bearer token is held in memory only. A `Logger.Info` line is
//     emitted on token refresh with the new expiry; the token itself
//     never leaves the process.
//   - HTTP traffic is logged at the (psp, method, path, status)
//     granularity. Request and response bodies are NEVER logged. The
//     only PII-adjacent field that leaves the process is the txid
//     (externalID).
//   - TLS 1.2 minimum. Certificate verification is the http.Transport
//     default (no InsecureSkipVerify). The client cert is loaded once
//     at construction so a missing or unreadable cert fails boot, not
//     a runtime request.
package inter
