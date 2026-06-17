# Self-hosted webfonts (Peitho — SIN-65087)

These woff2 files are served from disk at `/static/fonts/*.woff2` and wired
into the design system via `web/static/css/tokens.css` (`--font-sans`,
`--font-mono`, `@font-face`). They load under the strict CSP `font-src 'self'`
directive — **no Google Fonts CDN**, offline-safe.

## Files

| File | Family | Axis | Subset |
| --- | --- | --- | --- |
| `inter-latin-wght-normal.woff2` | Inter | wght 100–900 (variable) | latin |
| `inter-latin-ext-wght-normal.woff2` | Inter | wght 100–900 (variable) | latin-ext |
| `jetbrains-mono-latin-wght-normal.woff2` | JetBrains Mono | wght 100–900 (variable) | latin |
| `jetbrains-mono-latin-ext-wght-normal.woff2` | JetBrains Mono | wght 100–900 (variable) | latin-ext |

Variable fonts (single weight axis) keep the payload small while covering every
weight the UI uses (400/500/600/700). The `latin` + `latin-ext` split with
`unicode-range` in `@font-face` means a Brazilian-Portuguese page typically only
fetches the `latin` file.

## Provenance & license

Both families are licensed under the **SIL Open Font License 1.1** (OFL).

- **Inter** — Copyright 2016 The Inter Project Authors
  (<https://github.com/rsms/inter>). See `Inter-LICENSE.txt`.
- **JetBrains Mono** — Copyright 2020 The JetBrains Mono Project Authors
  (<https://github.com/JetBrains/JetBrainsMono>). See `JetBrainsMono-LICENSE.txt`.

woff2 binaries vendored from the `@fontsource-variable/{inter,jetbrains-mono}@5`
packages (jsDelivr CDN) — pre-built, subset, OFL-clean redistributions of the
upstream fonts. The Peitho design handoff (`design/peitho/`) specified these two
families but shipped only a Google-CDN `@import`; A2 self-hosts them per the
ticket's CSP / offline requirement.
