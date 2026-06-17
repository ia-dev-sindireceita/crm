# Peitho — Design System

> CRM B2B multi-tenant para equipes de vendas e atendimento que vivem 8 h/dia na ferramenta.
> **Prioridade absoluta do sistema visual: não cansar o olho.**

Peitho is a from-scratch brand and design system (no prior codebase or Figma was provided —
everything here was authored for this brief). The single global stylesheet consumers link is
**`styles.css`**, which `@import`s the token + base files under `tokens/`.

> **Nota técnica:** o namespace interno do bundle JS permaneceu `window.FluaDesignSystem_2587b4`
> (gerado pelo compilador e travado pelo identificador do projeto). É um identificador invisível
> ao usuário — toda a marca visível (logo, wordmark, copy) é **Peitho**.

---

## 1. Name — Peitho

**Peitho** (Πειθώ) é a deusa grega da **persuasão** — companheira de Afrodite, personificação do
convencimento e do encanto. Para um CRM de vendas o encaixe semântico é raro e preciso: **a
ferramenta existe para ajudar a equipe a persuadir e encantar clientes.**

- **Fonética:** *PÊI-tho* / *PAY-tho* (BR aceita "Peito" + "ho").
- **Domínio sugerido:** `peitho.io` / `peitho.com` (verificar registro; nomes mitológicos costumam ter mais disponibilidade que palavras comuns).
- **Por que funciona:** significado de marca embutido (persuasão = vendas), origem clássica que
  transmite credibilidade e sofisticação sem ser corporativo-genérico, sonoridade curta e
  memorável, e um monograma **"P"** limpo que escala de favicon a banner.

*Histórico:* nomes anteriores (Flua, Fluxia, Závio, etc.) foram descartados por indisponibilidade
de domínio. Peitho foi a escolha final do cliente.

---

## 2. Logo

Files in `assets/`:
- `peitho-icon.svg` — indigo rounded-square app icon + white **P** monogram. The favicon / app tile. Legible at **16 px**.
- `peitho-mark.svg` — the glyph alone, `currentColor` (monochrome; tint it any color).
- `peitho-logo-light.svg` — wordmark for light backgrounds (ink text).
- `peitho-logo-dark.svg` — wordmark for dark backgrounds (light text + lifted indigo tile).

**The mark:** a geometric **open-P** monogram — a single rounded stem with a perfect circular bowl
floating above it. The open counter keeps it light and modern (an all-day-friendly mark), the
circle nods to a classical seal/coin without being literal. No gradients; one flat indigo. Works on
light and dark, scales from favicon to banner. See the *Logo lockups* card in the Design System tab.

---

## 3. Content fundamentals — how Peitho writes

- **Language:** Portuguese (BR), product-first. Never explains "what a CRM is".
- **Voice:** calm, competent, persuasive-but-never-pushy. Short. Verb-led. We help you move faster and win, we don't cheer.
- **Person:** speaks **to** the user with implicit "você" ("Buscar conversa", "Novo lead"). Greets by first name ("Boa tarde, Rafael").
- **Casing:** **Sentence case** everywhere — buttons, menus, titles ("Novo negócio", not "Novo Negócio"). UPPERCASE only for tiny eyebrow/section labels (`.eyebrow`, 11px, tracked).
- **Labels:** nouns for nav ("Funil de vendas", "Contatos", "Inbox"), imperative verbs for actions ("Filtrar", "Adicionar", "Enviar").
- **Numbers:** BR format — `R$ 24.500`, `24,6%`. Always tabular (`.tnum`) in tables/metrics so columns align.
- **Status vocabulary (canonical):** *Ganho · Perdido · Em negociação · Qualificado · Novo · Aberto.*
- **Emoji:** essentially none in the UI chrome. Tolerated only inside user-generated message content (e.g. a customer's "🙌"). Never in labels, buttons, or marketing voice.
- **Tone examples:** "3 campanhas ativas" · "atualizados há instantes" · "Aqui está o desempenho da sua equipe hoje." · empty state: "Este módulo faz parte do Peitho. Conteúdo de exemplo em breve."

---

## 4. Visual foundations — why it doesn't tire the eye after 8 h

The whole system is tuned for **low arousal, high legibility, sustained reading**.

**Color**
- App background is a soft cool grey **`#F4F5F7`**, never pure white — kills the glare that fatigues eyes over hours.
- Panels/cards are white (`--surface`); nesting goes *up* in lightness, not down.
- Exactly **one** accent: a desaturated **indigo `#5B63D3`** for CTAs, links, selection, active nav. Vibrant enough to find, calm enough to stare at.
- Text is **`#1A1A2E`** (near-ink, not pure black) → softer contrast edge, less retinal buzz.
- Semantic colors are **muted** (sage green, dusty rose, ochre amber, slate blue) and almost always used as a **soft tint bg + darker fg pill**, never as large fills. No red/orange as a primary — only as the "Perdido/Pendente" status.
- **No gradients on backgrounds.** No neon. No bluish-purple hero gradients.

**Typography**
- **Inter** for all UI (superb at 12–14 px), **JetBrains Mono** for numbers/IDs.
- Compact scale: 14 px body, 13 px dense secondary, 12 px labels, 11 px eyebrows, 20 px+ only for page titles.
- Line-height **1.5** for body — generous breathing room for dense reading.
- Letter-spacing slightly negative on headings, wide on uppercase eyebrows.

**Spacing & density**
- Strict **4 px grid**. Density-medium — denser than a consumer app, looser than a spreadsheet (Linear / Notion territory).
- Default control height **32 px**; table rows ~46 px; sidebar items 34 px.

**Shape & elevation**
- Restrained radii: 7 px controls, 10 px cards, 14 px modals/⌘K, full-round only for pills/avatars.
- **Soft, low-opacity shadows** (`--shadow-sm/md`). Cards = `1px border + shadow-sm`. Hover lifts to `shadow-md` + stronger border. Never heavy drop-shadows.
- Dark mode leans on **borders + subtle surface lift** rather than big shadows.

**Motion & states**
- Quick, calm transitions: **120–180 ms**, ease / `cubic-bezier(.4,0,.2,1)`. No bounce, no springy overshoot, no infinite decorative loops.
- **Hover:** background tint shifts one step (ghost → `--bg-subtle`; primary → `--accent-hover`); icon/row reveals actions via opacity.
- **Press:** buttons nudge down ~0.5 px and scale to .985 — a tactile, quiet acknowledgement, not a big squish.
- **Focus:** indigo ring = 1 px accent border + 3 px `--accent-soft` glow.
- **Selection/active:** `--accent-soft` background + accent text/indicator.

**Imagery**
- The product surface is **chromeless and image-light** by design — data is the content. No stock photos, no illustration scenes in-app. Avatars are hashed-color initials (muted palette) unless a real photo exists.

**Dark mode** (`[data-theme="dark"]`)
- Background **`#0F1117`** (not pure black), surfaces `#171A22` / `#1E222C`, text `#E7E9F0`.
- Same accent, **lifted in luminosity** to `#6970DD`. Status tints rebuilt as low-luminance washes. Required, first-class — many users keep it on all day.

---

## 5. Iconography

- **System:** a curated subset of **[Lucide](https://lucide.dev)** (MIT) — 24×24 viewBox, **2 px stroke, round caps/joins**. Single consistent stroke family; no filled/duotone mixing (the one exception is the `circle` status dot).
- **Delivery:** shipped as a React `Icon` component (`components/core/Icon.jsx`) with an inline path map (`ICON_PATHS`) — self-contained, no CDN/runtime dependency, inherits `currentColor`, stroke scales with size. Default size **16 px** (CRM density); 17–20 px in nav, 14 px inline.
- **Why not a font / CDN:** an inline path map renders instantly with the bundle and never flashes or 404s in an offline doc. The set is small and intentional (search, plus, users, inbox, git-branch, zap, sparkles, bell, sun, moon, …) — see the *Iconography* card.
- **Unicode/emoji as icons:** no. Keyboard shortcuts use real glyph caps via the `Kbd` component (⌘, K, ↑, ↓, ↵).
- **Substitution flag:** these are faithful Lucide paths re-expressed for self-containment; if you need the full Lucide set, install `lucide-react` and the names map 1:1.

---

## 6. Index / manifest

**Root**
- `styles.css` — global entry (import list only).
- `readme.md` — this guide. · `SKILL.md` — Agent-Skills wrapper.
- `_ds_bundle.js`, `_ds_manifest.json` — generated by the compiler, do not edit.

**`tokens/`** — `fonts.css` · `colors.css` · `typography.css` · `spacing.css` · `elevation.css` · `base.css`

**`assets/`** — `peitho-icon.svg` · `peitho-mark.svg` · `peitho-logo-light.svg` · `peitho-logo-dark.svg`

**`components/`** (22 exports under `window.FluaDesignSystem_2587b4`)
- `core/` — Icon · Button · IconButton · Kbd
- `data-display/` — Badge · StatusBadge · Tag · Avatar · AvatarGroup
- `forms/` — Input · Select · Checkbox · Switch
- `surfaces/` — Card · Tabs · Tooltip
- `crm/` — **SidebarNav · LeadCard · FunnelRow · CommandBar** (+ StatusBadge) — the five signature components from the brief

**`ui_kits/crm/`** — interactive CRM recreation (Dashboard, Pipeline, Contacts, Inbox, Campaigns) with collapsible sidebar, ⌘K palette, and light/dark. Entry: `index.html`.

**`guidelines/`** — foundation specimen cards (Colors, Type, Spacing, Brand) shown in the Design System tab.

---

## ⚠️ Caveats
- **Internal JS namespace stayed `FluaDesignSystem_2587b4`** (compiler-locked to the project identifier). Invisible to users; all visible branding is Peitho. If a clean rename is required, recreate the project under the name "Peitho".
- **Fonts load via Google Fonts CDN** (`tokens/fonts.css`), not bundled binaries — so the compiler reports 0 local font-faces. For fully offline / self-hosted delivery, drop the `.woff2` files into `assets/fonts/` and swap the `@import` for `@font-face` rules. Flag to the user.
- Icons are a **Lucide subset re-expressed inline** (see §5), not the upstream package.
- The logo wordmark renders "Peitho" as live `<text>` in Inter; outline it to paths before sending to a printer that lacks the font.
