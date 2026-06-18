---
name: peitho-design
description: Use this skill to generate well-branded interfaces and assets for Peitho, either for production or throwaway prototypes/mocks/etc. Contains essential design guidelines, colors, type, fonts, assets, and UI kit components for prototyping.
user-invocable: true
---

Read the README.md file within this skill, and explore the other available files.
If creating visual artifacts (slides, mocks, throwaway prototypes, etc), copy assets out and create static HTML files for the user to view. If working on production code, you can copy assets and read the rules here to become an expert in designing with this brand.
If the user invokes this skill without any other guidance, ask them what they want to build or design, ask some questions, and act as an expert designer who outputs HTML artifacts _or_ production code, depending on the need.

## Quick facts
- **Peitho** (Greek goddess of persuasion) — B2B multi-tenant CRM for sales/support teams who live in the tool 8h/day. Design priority #1: **don't tire the eye**.
- **Accent:** one calm desaturated indigo `#5B63D3`. **App bg:** soft grey `#F4F5F7` (never pure white). **Ink:** `#1A1A2E`.
- **Type:** Inter (UI) + JetBrains Mono (numbers). 14px base, 1.5 line-height, compact density (4px grid, 32px controls).
- **Dark mode** is first-class: `[data-theme="dark"]`, bg `#0F1117`.
- **Voice:** PT-BR, sentence case, verb-led actions, no emoji in chrome.

## Files
- `styles.css` — link this; pulls in all tokens under `tokens/`.
- `readme.md` — full guide (name rationale, content + visual foundations, iconography, manifest).
- `components/` — 22 React primitives. The five signature ones live in `components/crm/`: SidebarNav, LeadCard, FunnelRow, StatusBadge, CommandBar (⌘K).
- `ui_kits/crm/` — interactive full-app recreation (open `index.html`).
- `assets/` — logo (`peitho-logo-light/dark.svg`), icon (`peitho-icon.svg`), monochrome mark (`peitho-mark.svg`).
- `guidelines/` — foundation specimen cards.

When mocking with components in a standalone HTML file, link `styles.css`, load `_ds_bundle.js`, then read components from `window.FluaDesignSystem_2587b4` inside a `<script type="text/babel">` (note: the JS namespace is an internal compiler-locked identifier and stays "Flua…" even though the brand is Peitho). For pure visual mocks you can also just use the CSS tokens directly.
