# SIN-66390 — Channel-management + per-channel access screens

**HTMX interaction spec** · Owner: UXDesigner · Parent: SIN-66378 · Blocks P2 + P4
Stack: **HTMX + Go templates** (server-rendered, `hx-*` partial swaps, progressive enhancement, no SPA/no client state machine). Strict CSP — **no inline `on*=` / `hx-on:` handlers; use OOB swaps**.

Reference surface: `internal/web/branding` (iamRoutes wireup, `RequireAction` gerente gate, `hx-post` → `hx-swap=outerHTML` partials, `hx-swap-oob` for cross-region updates). Sheet order in `<head>`: **`tokens.css` → `components.css` → `channels.css`**; dark via `[data-theme="dark"]`; htmx vendored + nonce'd (`/static/vendor/htmx/2.0.9/htmx.min.js`) — the surface loads its own htmx (shell does not inject it).

---

## 0. Design decisions ratified into this spec

| # | Decision | Lens / rationale |
|---|----------|------------------|
| D1 | **Deactivate, never destructive-delete.** A channel with conversation history is only ever soft-deactivated (reversible). | Forgiveness; avoids orphaning inbox threads. |
| D2 | **Access roster is a first-class, editable section *inside* the create/edit form**, pre-checked to all active tenant users. (Option-A refinement, ratified 2026-06-30.) | Safe default (no unseen-message hazard) + least-privilege one deselect away + consciously chosen. Recognition > Recall. |
| D3 | **Backfill = grant-all** to current users on existing channels (zero regression). New-channel default = the in-form roster of D2, not a separate first-touch screen. | Endorses CTO Option-A confirmation `7c72423f`. |
| D4 | **Masked identity** everywhere (`+55 ·· ····-··88`, `@····dler`). Full identity never rendered in list/roster. | Data minimization (LGPD); identity is not a secret but defaults to masked in operator views. |
| D5 | WhatsApp QR / pairing lives behind a **per-row "Conectar" affordance** (progressive disclosure), never inside the create form. | Progressive Disclosure; the create form stays a single conceptual job. |

---

## 1. Route + RBAC map

| Method · Route | Surface | Gate (`RequireAction`) | Swap target |
|---|---|---|---|
| `GET /settings/channels` | Registry (full page) | `ActionTenantChannelsManage` (gerente) | — |
| `GET /settings/channels/new` | Create form (full page or modal partial) | gerente | `#channels-modal` |
| `POST /settings/channels` | Create submit | gerente | `#channels-list` (OOB row insert) |
| `GET /settings/channels/{id}/edit` | Edit form | gerente | `#channels-modal` |
| `POST /settings/channels/{id}` | Edit submit | gerente | `#channel-row-{id}` (`outerHTML`) |
| `POST /settings/channels/{id}/active` | Activate / deactivate toggle | gerente | `#channel-row-{id}` (`outerHTML`) |
| `GET /settings/channels/{id}/access` | Per-channel access maintenance (P3) | gerente | — / `#access-roster` |
| `POST /settings/channels/{id}/access` | Roster save | gerente | `#access-summary-{id}` (OOB) + `#access-roster` |

**Wireup hard rules (do not let impl skip):**
- Mount via the **`iamRoutes` slice** (`iam_wire.go` → `main.go`), NOT the `httpapi/router.go` `deps.X != nil` block. A web `/settings/*` surface that 404s on an authed route = missing `iamRoutes` entry, not a cache/auth issue. → `[[reference_crm_web_surface_mount_via_iamroutes]]`
- **chi route-enumeration trap:** every new POST route (`/active`, `/access`) needs its own `authed.Method(...)` registration. chi enumerates inbox/settings routes one-by-one — a missing entry yields chi `404 page not found` while the button still renders and inner-mux tests pass. → `[[reference_crm_inbox_chi_route_enumeration_trap]]`
- **Sync `buildHelloSurfaces`** so post-login hello-tenant lands a link to `/settings/channels` for gerente. → `[[feedback_hello_tenant_sync_on_mount]]`
- **Seed-role check:** confirm the gerente seed user resolves `ActionTenantChannelsManage` before shipping, or first login 403s. → `[[feedback_role_gate_ack_check_seed]]`

---

## 2. Screen 1 — `/settings/channels` registry (gerente)

**JTBD:** "As gerente I want to see every channel my tenant receives messages on, who can work each one, and turn one off without losing its history."

### Layout (desktop 1440, primary)
Single-column page inside app-shell `<main>`. `.card`-framed table region. Page header row: `<h1>` "Canais" (`--text-2xl`/`--font-semibold`) + right-aligned **primary** action `<a class="btn btn--primary" hx-get="/settings/channels/new" hx-target="#channels-modal" hx-swap="innerHTML">+ Novo canal</a>`. F-pattern: title top-left, primary CTA top-right (Fitts: large, corner-anchored).

### Table (`.table` inside `.card`, `<table id="channels-list">`)
| Col | Content | Notes |
|---|---|---|
| Canal | channel SVG glyph (reuse inbox `.channel-badge--whatsapp` set) + display name (`--font-medium`) | glyph is decorative, name carries the label |
| Tipo | type label (WhatsApp / Telegram / Instagram / Webchat / E-mail) | `--text-muted` |
| Identidade | masked identity, `--font-mono` `--text-sm` | D4; e.g. `+55 ·· ····-··88` |
| Acesso | access summary chip: **"Todos"** `.badge` (neutral) or **"N atendentes"** `.badge--info` | links to access view |
| Status | **Ativo** `.badge--success` / **Inativo** `.badge` (neutral gray, NOT danger — inactive is a valid state, not an error) | color-independent: text label always present |
| Ações | `Editar` (`.btn--ghost`) · `Ativar`/`Desativar` toggle · `Conectar` (WhatsApp only, when unpaired) | each ≥44×44 hit target |

Row id `channel-row-{id}` so toggle/edit swap `outerHTML` in place. Single-column zebra-free table; align numeric/identity left (mono). Serial-position: most-used columns (Canal, Status) at the scan edges.

### States (all get equal craft — D-bar)
1. **Empty registry** — `.empty-state`: title "Nenhum canal configurado", body "Configure o primeiro canal para começar a receber mensagens.", primary `+ Novo canal` CTA inside the empty state. No bare table headers over emptiness.
2. **Single channel** — table renders one row; no special-case layout.
3. **Many channels** — table scrolls with the page (operator long-session desktop density); sticky `<thead>` via `position: sticky`.
4. **Deactivated channel** — row stays, `Inativo` badge, identity + access muted (`opacity` not applied — use `--text-muted`), action flips to `Ativar`. Row is not hidden (Recognition: gerente must see it exists to re-enable).
5. **Connection-pending (WhatsApp unpaired)** — `Conectando…` `.badge--warning` + `Conectar` ghost action; never blocks the rest of the table.

### Feedback
- Toggle returns the swapped row + an OOB `.alert--success` toast in `#channels-toast` ("Canal desativado. As conversas existentes permanecem."). `hx-indicator` spinner on the toggle button during the <400ms swap (Doherty).

---

## 3. Screen 2 — Create / edit channel form (gerente) — **with in-form access roster (D2)**

Rendered into `#channels-modal` as `<div class="modal">` (`.modal__dialog`, `.modal__title`, `.modal__actions`). Focus moves to the dialog on open; `Esc` closes (anchor/JS-free fallback: form is also reachable as the full page `GET /settings/channels/new`). Single-column form (`.field` stack) — never multi-column form fields.

### Section A — Channel identity (`.card__body`)
- `.field` **Nome de exibição** (`field__input`, required, autofocus). Help: "Como este canal aparece para a equipe."
- `.field` **Tipo** (`field__select`: WhatsApp / Telegram / Instagram / Webchat / E-mail). On change → htmx `hx-get` swaps the **identity sub-field** partial into `#channel-identity-field` (type-specific: phone for WhatsApp, handle for Telegram/IG, address for e-mail). Server-driven, no client branching.
- `.field` **Identidade** (type-dependent input, masked on blur). Server validates format; inline `.field__error` names the fix ("Informe o número com DDI, ex: +55 11 ·····-····").

### Section B — Acesso da equipe (roster primitive, first-class)
Header: "Quem atende este canal" + helper `.field__help`: "Todos os atendentes ativos começam marcados. Desmarque quem não deve ver estas conversas." Pre-checked = all active users (D2/D3).

Roster = `<fieldset id="access-roster">` of checkbox rows (the **shared primitive** — see §5). Bulk controls: **"Marcar todos"** / **"Desmarcar todos"** (`.btn--ghost`, server-side `hx-post` re-render of the fieldset, OOB count update — no inline JS). Live count line (`aria-live="polite"`): "N de M atendentes com acesso".

> **Ethics gate:** pre-checking is a *safe* default (no information hidden from the operator who creates it), not a dark pattern — every box is visible, labeled, and one click from off, and the count is always shown. This is Defaults done right, not sneak-into-basket.

### `.modal__actions`
- Primary `Salvar canal` (`.btn--primary`, `hx-post`). On success: modal closes via OOB empty `#channels-modal`, new/updated row OOB-swapped into `#channels-list`, success toast.
- Secondary `Cancelar` (`.btn--ghost`, closes modal, no mutation — Forgiveness).
- Destructive **deactivate** is NOT in this form (it's the row toggle) — keeps create/edit non-destructive.

### Validation & errors
- Server-side, returned as the re-rendered form partial with `.field__error` per field + a summary `.alert--danger` at top of dialog (`role="alert"`, focus moved to it). Postel: trim/normalize identity server-side, reject only what's truly invalid.

### Edit variant
Same form, pre-filled. Type is **read-only** on edit (changing a live channel's type would orphan its conversations — constraint, not a choice). Roster pre-checked to current membership (Recognition).

---

## 4. Screen 3 — Per-channel access maintenance (P3 host)

`GET /settings/channels/{id}/access` — full page reusing the **roster primitive** (§5) standalone. Header: channel name + masked identity + type glyph (context anchor so gerente knows which channel they're tuning). Same fieldset, bulk controls, live count. `Salvar acesso` primary → OOB update of the registry row's access summary chip (`#access-summary-{id}`) + success toast.

- Empty assignable-users state: `.empty-state` "Nenhum atendente ativo neste tenant" + link to user management (no broken checkbox list over emptiness).
- **SecurityEngineer loop-in (required):** this is the tenant's first **per-resource authz** UX. Confirm with SecurityEngineer: (a) gerente-only mutate, (b) atendente cannot self-grant, (c) audit-log line on every access change, (d) deactivating a user elsewhere cascades to roster display (no stale "ghost" access). Spec defers the authz *enforcement* contract to SecurityEngineer; UX guarantees the gerente sees an accurate, current roster.

---

## 5. Roster primitive (shared by Screen 2 §B and Screen 3)

`<fieldset id="access-roster">` — one `.field-tier__row`-style checkbox row per active tenant user:
- `<input type="checkbox" name="user_ids" value="{userId}">` (≥44px row hit target via padding, not tiny box)
- user display name (`--font-medium`) + role label (`--text-muted` `--text-xs`)
- checked = has access. Recognition over Recall: current state pre-rendered, never "start blank".
- Keyboard: native checkbox, Tab-reachable, Space toggles, visible `:focus-visible` outline (token).
- Bulk "Marcar/Desmarcar todos" submit server-side and OOB-swap the fieldset + count (no `hx-on`, no inline JS — CSP).
- Live region `<p aria-live="polite" id="access-count">N de M com acesso</p>`.

This is the single source of truth for both P2 (create form) and P3 (maintenance) — implement once, mount twice.

---

## 6. Screen 4 — Inbox channel-scope chip (consumed by P4)

Reuse the existing `.inbox-filters__pill` primitive (already in `inbox.css`). In the inbox filter bar, add a **channel-scope indicator**:
- When an atendente's listing is membership-filtered, show a non-interactive indicator pill: `.inbox-filters__pill` (info variant) "Canais: 2 de 4" with a `title`/`aria-label` "Você vê apenas conversas dos canais aos quais tem acesso." — prevents the "missing threads" confusion (Mental Model: explain the absence).
- When gerente filters by a specific channel, the pill becomes an **active filter chip** (`.is-active`) with a dismiss `×` (`hx-get` re-renders the unfiltered list, OOB count). Fitts: dismiss target ≥44px.
- Membership-filtered **empty state** (atendente has access but no threads): `.conversation-list__empty--filtered` "Nenhuma conversa nos seus canais" — distinct copy from the no-access case.

P4 consumes the chip token/class names here; no new tokens required (reuses inbox filter primitives).

---

## 7. `channels.css` — proposed tokens & classes

No new design tokens required — everything composes from `tokens.css`. New surface classes (all token-based, AA-checked against `--surface-1` app bg, not white — `[[reference_htmx_visual_truth_gate_recipe]]` Peitho gotcha):

```
.channels-page            /* layout wrapper, max-width + --space-5 gutters */
.channels-page__header    /* flex: title <-> primary CTA */
.channels-list            /* extends .table; sticky thead */
.channels-row--inactive   /* --text-muted identity/access cells */
.channels-access-summary  /* .badge wrapper, link affordance */
.channels-roster          /* fieldset; --space-2 row gap */
.channels-roster__row     /* ≥44px hit target, checkbox + label + role */
.channels-roster__bulk    /* .btn--ghost group */
.channels-roster__count   /* aria-live count line, --text-muted --text-sm */
.channels-connect         /* WhatsApp pairing affordance, .badge--warning + ghost btn */
```

Dark mode: `channels.css` is authored token-only so it inherits whatever dark flip the token layer provides — **no per-class dark overrides**. ⚠️ **System finding (verified 2026-06-30):** the shipped `tokens.css`/`components.css` currently contain **no `[data-theme="dark"]` block and no `prefers-color-scheme`** — there is no dark token set in the repo today. The issue's "dark via `[data-theme="dark"]`" convention is therefore a *forward contract* (channels.css must not hardcode any color so it flips for free once dark tokens land), not a currently-verifiable state. Raised separately so the design-system owner can confirm whether dark tokens are planned; it does not block this surface. Load order pinned: `tokens.css` → `components.css` → `channels.css`.

---

## 8. Accessibility acceptance criteria (WCAG 2.1 AA)

- Semantic HTML first: `<table>` for registry, `<fieldset>`/`<legend>` for roster, `<label>` bound to every input, `<button>`/`<a>` for actions (never div-with-handler).
- Color-independent status: every badge carries a text label; never color-only.
- Contrast ≥ 4.5:1 body / 3:1 large — verified against the actual surface the text sits on.
- Target size ≥ 44×44 for every interactive element (rows, toggles, checkboxes, dismiss).
- Visible `:focus-visible` (token outline) on all interactives; logical Tab order: header CTA → table rows (action-by-action) → modal traps focus when open.
- `aria-live="polite"` on roster count + toast region; `role="alert"` on validation summary with focus moved to it.
- Reduced motion respected (tokens already zero `--motion-*` under `prefers-reduced-motion`).
- No-JS fallback: create form reachable as full page; toggle/access submit as standard form POST (htmx is progressive enhancement only).

---

## 9. Handoff

- **Coder (HTMX/Go templates)** — implements P2 (Screens 1–2 + roster primitive) and P3 (Screen 3) per this spec; P4 (inbox chip) reuses §6. Named tokens/components above are the contract. Watch iamRoutes + chi-enumeration + hello-tenant sync (§1).
- **SecurityEngineer** — loop in on Screen 3 / roster authz (§4) before P3 ships: per-resource gerente-only mutate, no atendente self-grant, audit line, user-deactivation cascade.
- **System-level note:** no new tokens proposed; `channels.css` is additive. Roster primitive is a reusable component — register it in the component inventory so future per-resource access surfaces inherit it.

## 10. Visual-truth gate (prototype)

Static prototype of Screen 1 (registry, all 5 states) + Screen 2 (create form with in-form roster) built against the **real shipped `tokens.css` + `components.css`** plus draft `channels.css`, rendered via the no-Docker Playwright + axe + keyboard recipe (`[[reference_htmx_visual_truth_gate_recipe]]`) at **1440×900 desktop**.

**Result: PASS.**
- axe-core (wcag2a/2aa/21a/21aa): **0 serious/critical violations** on both screens.
- Keyboard tab order verified logical: header CTA → row actions in order → (form) name → type → identity → bulk → roster checkboxes → cancel/save. Native checkboxes Space-toggle; visible focus ring (token outline) on the autofocused name field (see screenshot).
- Visual craft confirmed: hierarchy legible in <2s, token spacing, badge semantics color-independent (every status carries a text label), masked mono identity.
- **Dark theme NOT verifiable** — no dark token set exists in the repo (see §7 finding); the "dark" probe pass is vacuous (nothing flips). Light is the only shipped theme; gate claim is scoped to light.

Screenshots attached to the issue: `shot-registry-light.png`, `shot-create-light.png`.

The impl PRs (P2/P3) **must re-run this gate against the real rendered Go templates** at 1440×900 desktop before `done` (the prototype proves the spec is renderable + AA, not that the implementation is).
