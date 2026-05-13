// Package vendorassets is a thin embed bridge that exposes
// web/static/vendor/CHECKSUMS.txt to Go callers without dragging the
// vendored JS payloads themselves into the binary. The SRI parser
// lives in internal/web/vendor; this package only owns the embed
// directive because //go:embed must be in a Go source file that sits
// at or above the embedded file in the directory tree.
package vendorassets

import "embed"

// ChecksumsFS exposes CHECKSUMS.txt as a read-only embedded filesystem.
// Consumers should treat the manifest path as "CHECKSUMS.txt".
//
//go:embed CHECKSUMS.txt
var ChecksumsFS embed.FS

// ChecksumsManifestPath is the canonical path within [ChecksumsFS].
const ChecksumsManifestPath = "CHECKSUMS.txt"
