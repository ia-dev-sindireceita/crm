# ADR 0060 — Palette extraction: median-cut via `cenkalti/dominantcolor`

- Status: Accepted
- Date: 2026-05-19
- Drives: [SIN-63072](/SIN/issues/SIN-63072) (this ADR), [SIN-62198](/SIN/issues/SIN-62198) (Fase 5)
- Unblocks: [SIN-63076](/SIN/issues/SIN-63076) (`internal/branding` implementation),
  [SIN-63075](/SIN/issues/SIN-63075) (migration `tenant_palette`)
- Related: [ADR 0080](0080-uploads.md) (upload format/size policy)

## Context

Fase 5 ([SIN-62198](/SIN/issues/SIN-62198)) introduces white-label per
tenant: the tenant uploads a logo, the system extracts a colour
palette, derives CSS variables (`ThemeTokens`), and applies them at
render time. The Fase 5 parent fixes:

- `internal/branding` owns `Palette` + `ThemeTokens` and the
  `PaletteExtractor` port,
- the implementation must be swappable,
- WCAG AA contrast (≥ 4.5:1 for normal text) is a hard requirement,
- manual override + 30-day revert are required (lives outside this ADR).

[ADR 0080](0080-uploads.md) already locks the upload contract for
tenant logos: PNG, JPEG, or WEBP; ≤ 2 MB; ≤ 1024 × 1024. SVG is
rejected at the upload boundary. This ADR inherits those limits and
does not re-open them.

The acceptance criteria for [SIN-63072](/SIN/issues/SIN-63072) ask the
CTO to pick the algorithm and the Go library (or external service),
specify the port, and document the WCAG fallback. Implementation is
[SIN-63076](/SIN/issues/SIN-63076); this PR ships only the ADR and the
port stub.

## Decision

**Palette extraction uses median-cut quantisation via
`github.com/cenkalti/dominantcolor` on a `≤ 256 × 256` resampled
copy of the logo, with a deterministic WCAG AA fallback when the
extracted primary cannot reach 4.5:1 against either pure white or
near-black text.**

The domain port lives at `internal/branding`:

```go
type PaletteExtractor interface {
    Extract(ctx context.Context, src io.Reader, hint Hint) (Palette, error)
}
```

A single adapter — `internal/adapter/branding/mediancut` — implements
the port using `dominantcolor`. Manual overrides, persistence, and
`ThemeTokens` CSS rendering live in producer packages and are out of
scope here.

## Rationale

### Why median-cut, not k-means or octree

Tenant logos are not photographs. They are typically:

- flat colour fields with a small number of brand colours,
- vector art rasterised to PNG/WEBP with crisp edges,
- often containing large white/transparent regions.

For that input class, median-cut behaves better than k-means and
octree:

1. **Median-cut is deterministic.** Given the same image, it always
   returns the same palette. Tests can pin exact RGB triples without
   approximate-equality helpers. K-means depends on initial centroid
   seeding and converges to local optima — different starts yield
   different palettes for the same logo. We would either pin a seed
   (brittle when the lib version changes) or accept colour drift on
   library upgrades. Deterministic output also matters for the
   "revert to default" UX: the same logo must always derive the same
   default palette so "revert" is well-defined.
2. **No iteration tuning.** K-means needs `k`, an iteration cap, and
   a tolerance. Median-cut needs only `k`. We expose `k = 5` to the
   port; everything else is fixed.
3. **Stable on flat-field input.** Median-cut splits the colour cube
   by variance, which is robust to large monochrome backgrounds
   (typical for logos with a white plate). K-means is sensitive to
   the dominant cluster and tends to return five shades of background
   when the brand colour is < 5% of pixels.
4. **Cheaper.** `O(n)` over the pixel set per split level vs. k-means
   `O(n · k · iters)`. On a 256 × 256 image (65 k pixels) with k = 5,
   median-cut completes in well under 10 ms p50 on a single core.

Octree quantisation is comparable to median-cut on quality but is
more code with no advantage on this input class.

### Why `cenkalti/dominantcolor`

The candidate set is narrow once we constrain to "pure Go, no CGO,
permissive licence, maintained":

| Library | Algorithm | License | Pure Go | Notes |
|---|---|---|---|---|
| `github.com/cenkalti/dominantcolor`        | Median-cut | MIT       | yes | Tiny (~200 LoC), zero transitive deps, well-known author (`backoff`). |
| `github.com/EdlinOrg/prominentcolor`       | K-means    | **GPL-2.0** | yes | Licence is a closed-source blocker. |
| `github.com/generaltso/vibrant`            | Material-You-style | Apache-2.0 | yes | Larger surface, pulls helpers we don't need. |
| `github.com/RobCherry/vibrant`             | Material-You-style | MIT       | yes | Maintained sporadically; broader API than we need. |
| `github.com/marekm4/color-extractor`       | Custom histogram | MIT | yes | Returns hex strings, not `color.Color`; quality on flat-field logos was visibly worse than median-cut in the prior art we surveyed. |
| Hand-rolled median-cut                     | Median-cut | n/a      | n/a | ~200 LoC + tests. Worth it only if a third-party lib fails review; see "Reversibility". |

**Choice: `github.com/cenkalti/dominantcolor`.**

- **License**: MIT is compatible with the proprietary core.
- **Supply chain**: no transitive runtime deps. The package imports
  only stdlib (`image`, `image/color`, `sort`). `govulncheck` and the
  SBOM surface barely move.
- **Algorithm**: median-cut, matching the rationale above.
- **Boring-tech budget** (project rule): a 200-LoC pinned-version
  third-party median-cut is more boring than re-implementing it
  in-tree. The cost of "one more direct dep" is offset by the
  testing burden we avoid.
- **API fit**: returns `[]color.Color` from `image.Image`, which is
  exactly what the adapter needs after `image.Decode`.

### Why not an external service

Imagga, Cloudinary, and Google Vision all expose colour-extraction
endpoints. Rejected because:

1. Network latency turns logo upload into a multi-second flow with a
   spinner; median-cut on a 256 × 256 image finishes in < 10 ms.
2. Cost per tenant is non-zero on top of an already-zero local cost.
3. Vendor lock-in inverts our boring-tech bias.
4. PII surface widens: tenant logos can contain trademarked artwork
   that we should not ship to a third party without explicit consent.
5. The port stays the same regardless, so a future shift to a service
   adapter remains possible without touching the domain.

### WCAG AA policy (foreground/background contrast)

WCAG 2.x defines relative luminance `L` for an sRGB triple `(R, G,
B)` (with channels in 0..1):

```
L = 0.2126 · f(R) + 0.7152 · f(G) + 0.0722 · f(B)
f(c) = c/12.92                       if c ≤ 0.03928
f(c) = ((c + 0.055)/1.055) ^ 2.4     otherwise
```

Contrast ratio between two colours with luminance `L1 ≥ L2`:

```
CR = (L1 + 0.05) / (L2 + 0.05)        ∈ [1.0, 21.0]
```

The extractor returns five slots (`Primary`, `Secondary`, `Accent`,
`Foreground`, `Background`). Producer code paints text in
`Foreground` on a `Background` plate and CTA labels in
`text-on-primary` on a `Primary` plate. Both pairs must satisfy
`CR ≥ 4.5` (WCAG AA, normal text).

The extractor enforces the policy as follows:

1. Run median-cut → ranked `[]RGB` of length `k = 5`.
2. Pick **Primary** = the most dominant non-near-neutral colour
   (skip any RGB whose distance to the nearest of `#000` / `#FFF` is
   below ε = 0.04 in sRGB cube), falling back to the most dominant
   colour if all are neutral.
3. Pick **Secondary** = the next-most-dominant colour with hue
   distance > 30° from Primary, else next-most-dominant.
4. Pick **Accent** = the highest-saturation remaining colour.
5. Choose **Foreground** ∈ { `#0F1115`, `#FFFFFF` } to maximise
   contrast against **Background** = `#FFFFFF`. Same applies for
   `text-on-primary`: pick whichever of `#0F1115` / `#FFFFFF` yields
   the higher contrast against **Primary**.
6. **Fallback when Primary alone cannot reach 4.5:1**: if neither
   `#0F1115` nor `#FFFFFF` reaches `CR ≥ 4.5` against the chosen
   Primary (rare — only mid-grey primaries near `#7F7F7F` fail this
   on both sides), darken Primary in HSL by steps of `ΔL = −0.05`
   (capped at 6 steps) until the higher-contrast text candidate
   crosses 4.5:1. If still unmet, replace Primary with the
   deterministic neutral pair `#1F2937` / `#FFFFFF` and set
   `Source = PaletteSourceFallback`.
7. Background is always `#FFFFFF` for v1. Dark-mode background
   selection is out of scope (Fase 6 candidate).

The adjustment is bounded (≤ 6 HSL steps) and deterministic; the same
logo always yields the same Primary modulo this clamp. The chosen
text colour is recorded on the `Palette` so producers don't recompute
contrast at render time.

### Sizing and timing

The extraction pipeline:

1. **Decode**: `image.Decode` from `image/png`, `image/jpeg`, or
   `golang.org/x/image/webp` (already in `go.mod`). The hint
   `Content-Type` is advisory; the decoder is selected by the
   registered magic-byte sniff (already validated upstream by ADR
   0080).
2. **Resample**: `golang.org/x/image/draw.NearestNeighbor` to fit
   inside a 256 × 256 box, preserving aspect ratio. Nearest-neighbor
   is intentional — bilinear bleeds anti-alias artefacts into
   solid-colour regions and shifts palette dominance.
3. **Alpha skip**: `dominantcolor.Find` is given a sub-image whose
   transparent pixels (alpha < 16/255) are masked to a sentinel that
   the lib's frequency count ignores. (Implementation detail: use a
   `color.NRGBA` copy and pre-filter; the lib itself does not skip
   alpha.)
4. **Quantise**: `dominantcolor.Find(img, 5)`.
5. **Slot + WCAG-fit** as above.

Target: **p99 ≤ 100 ms** for a 1024 × 1024 PNG on a single core.
Resample + median-cut on 256 × 256 is the dominant cost; both are
linear in pixels.

The port runs synchronously inside the logo upload handler. If a
future workload (batch tenant import) needs async extraction, that is
a producer concern, not a port change.

## Consequences

### Positive

- One new direct dependency (`github.com/cenkalti/dominantcolor`),
  zero transitive runtime deps; `govulncheck`/SBOM impact is one
  line.
- Algorithm is pure → unit-testable without I/O. Tests pin exact
  RGB outputs because the algorithm is deterministic (see "Test
  fixtures" below).
- Domain has no dependency on the chosen library. Swapping to a
  service adapter or to a hand-rolled algorithm later is a sibling
  package, not a refactor.
- WCAG AA is enforced at the boundary; producers do not need to
  recompute contrast.
- Extraction cost stays well inside the upload handler's existing
  budget.

### Negative

- Median-cut is not state-of-the-art for photographs. If a future
  product line requires palette extraction from user-shot photos
  (campaign hero images, etc.), revisit — possibly with a Material-
  You-style salience pass on top of median-cut, or a separate
  port.
- The five-slot Palette is opinionated. Tenants wanting more (e.g.
  separate hover / active CTA colours) get them via manual override,
  not via extraction.
- Mid-grey-only logos (rare) fall through to the deterministic
  fallback pair. The UI must surface "couldn't derive a unique
  brand colour, using neutral default — adjust manually" instead of
  silently shipping a generic palette.

### Operational

- No new env vars. No new infra.
- New direct dep added to `go.mod` in the implementation PR
  ([SIN-63076](/SIN/issues/SIN-63076)).
- Metric: `branding_palette_extraction_seconds` histogram, label
  `outcome={extracted,fallback,error}`.
- Audit: log `palette.source=extracted|fallback` plus the resulting
  hex codes (no PII in the logo, but log size matters; ~80 bytes per
  upload).

### Out of scope (deliberate)

- Persistence of the palette (`tenant_palette` table is
  [SIN-63075](/SIN/issues/SIN-63075)).
- CSS variable rendering and HTMX swap path (producer concern).
- Manual override UI and 30-day revert (Fase 5 producer issue).
- Dark-mode palette derivation (Fase 6 candidate).
- Custom Foreground/Background overrides per CTA state.

## Test fixtures (minimum set)

Implementation ([SIN-63076](/SIN/issues/SIN-63076)) MUST include
unit tests against this fixture set, each pinning the expected
`Palette` (5 RGB triples + `Source`):

1. **High-variance brand** — Pitho corporate red / orange /
   yellow on white (multiple distinct brand colours).
2. **Single-colour wordmark** — dark navy text on transparent
   background (transparency edge).
3. **Monochrome black logo on white** — exercises the "primary is
   near-neutral" branch.
4. **Pastel low-contrast** — extracted primary fails 4.5:1 against
   both `#0F1115` and `#FFFFFF`, triggers HSL darken loop.
5. **Mid-grey logo** — triggers the full fallback to `#1F2937` /
   `#FFFFFF`, `Source = PaletteSourceFallback`.
6. **WEBP with alpha gradient** — exercises the decode path and
   alpha-skip filter.
7. **Tiny icon** (64 × 64 PNG) — exercises the no-upscale path
   in the resampler.

Each fixture lives in `internal/branding/testdata/`. Fixtures are
checked-in PNGs/WEBPs; the test reads them with `os.ReadFile` and
calls `Extract` against a tablename-driven matrix. Tests must not
depend on float-equality helpers — the port returns `uint8` channels.

## Rollback

Two rollback levels:

1. **Disable extraction** without removing the port: producer falls
   back to a hard-coded default palette (the same `#1F2937` /
   `#FFFFFF` pair) on `errors.Is(err, branding.ErrUnavailable)`. The
   adapter factory can be wired to return an "unavailable" stub
   without changing call sites.
2. **Revert the implementation PR**: removes
   `internal/adapter/branding/mediancut` and the `dominantcolor`
   dep. The port stays; producers compile against the default
   palette. No data migration involved (palette is derived, not
   stored under this ADR).

## Alternatives considered

- **K-means (e.g. `prominentcolor`)** — rejected on licence
  (GPL-2.0) and on determinism grounds.
- **Octree quantisation** — comparable quality, more code, no
  popular pure-Go lib that beats `dominantcolor`'s footprint.
- **`generaltso/vibrant` (Material-You)** — over-specified for our
  five-slot output; pulls helpers we don't need.
- **External service** (Imagga / Cloudinary / Vision) — rejected on
  latency, cost, vendor lock-in, and PII surface.
- **Hand-rolled median-cut in `internal/branding`** — viable, but
  duplicates `dominantcolor`'s 200 LoC with no measurable benefit.
  Kept as the "second-best fallback" if the dep ever fails review.

## References

- WCAG 2.2, [Success Criterion 1.4.3 Contrast (Minimum)](https://www.w3.org/TR/WCAG22/#contrast-minimum)
- Median-cut: Heckbert, "Color Image Quantization for Frame Buffer
  Display" (SIGGRAPH '82).
- `github.com/cenkalti/dominantcolor` (MIT).
