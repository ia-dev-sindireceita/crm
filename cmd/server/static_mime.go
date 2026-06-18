package main

import "mime"

// SIN-65088 — guarantee the Peitho brand assets are served with the
// correct Content-Type even on minimal deploy images that ship without a
// system MIME database (/etc/mime.types). http.FileServer otherwise
// falls back to content sniffing, which mistypes SVG as text/xml and the
// web manifest as text/plain — breaking <img>, <link rel="icon"> and the
// install/app-tile manifest in the browser.
//
// Registering at package init keeps both the production FileServer
// (cmd/server) and the cookieless static origin in agreement, and makes
// the registration effective in tests too.
func init() {
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}
