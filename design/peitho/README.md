# Peitho design handoff (vendored)

This directory vendors the **Peitho** design-system handoff bundle so later tranches
(B/C/D of [SIN-65082](https://github.com/pericles-luz/crm) — Peitho foundations) can
extract exact tokens, fonts, icons, brand SVGs and read the per-screen CRM mocks
straight out of the repo.

## Provenance

- **Source:** attachment `e6a89d0e-94be-44e2-8f7f-72bf0559f1bf` ("Flua Design System-handoff.zip")
  on Paperclip issue **SIN-65081** (Peitho rebrand + design-system epic).
- **Origin tool:** exported from Claude Design (`claude.ai/design`).
- **Vendored by:** SIN-65086 (Tranche A1) — pure additive handoff, **no app behavior change**.

The bundle is preserved **exactly as exported** under [`flua-design-system/`](./flua-design-system/);
its original folder structure is intact so assets can be copied out verbatim. Only this
top-level `README.md` was added.

## Naming: brand is Peitho, JS namespace stays `FluaDesignSystem_*`

- The **user-visible brand is Peitho** (Greek goddess of persuasion). All product copy,
  logos and assets use "Peitho".
- The bundle's **internal JavaScript namespace is `FluaDesignSystem_2587b4`** (e.g.
  `window.FluaDesignSystem_2587b4` in the UI-kit JSX). This is a compiler-locked internal
  identifier — **do not rename it.** It is an implementation detail of the exported bundle,
  not a brand name.

## What's inside

- `flua-design-system/project/tokens/` — CSS custom-property tokens (colors, spacing,
  typography, elevation, fonts, base). `styles.css` links them all.
- `flua-design-system/project/SKILL.md` — design skill / quick-facts guide for the brand.
- `flua-design-system/project/readme.md` — full brand guide (foundations, iconography, manifest).
- `flua-design-system/project/assets/` — brand SVGs (`peitho-logo-light/dark.svg`,
  `peitho-icon.svg`, `peitho-mark.svg`).
- `flua-design-system/project/components/` — 22 React component specs (`.jsx` + `.d.ts` +
  specimen `.card.html`), including the signature CRM primitives under `components/crm/`.
- `flua-design-system/project/guidelines/` — foundation specimen cards (colors, type, spacing).
- `flua-design-system/project/ui_kits/crm/` — interactive full-app CRM mock (open `index.html`):
  AppShell, Dashboard, Inbox, Pipeline, Contacts and misc views.

## Scope note

This is vendored **design source only**. Nothing here is wired into the application —
no `web/`, `internal/`, or `cmd/` changes, no `tokens.css` edits, no template edits.
Later tranches reference these assets. Rollback is a trivial revert of this additive directory.
