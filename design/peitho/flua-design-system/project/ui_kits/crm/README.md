# Peitho — CRM UI kit

Interactive, high-fidelity recreation of the Peitho CRM app. Composes the design-system
component primitives (it does **not** re-implement them).

## Run
Open `index.html`. It loads `styles.css`, the compiled `_ds_bundle.js`, then the view
files. Try: navigate the sidebar, collapse it, toggle dark mode (top-right), press **⌘K**.

## Files
- `index.html` — mounts `AppShell`; load order: bundle → `data.js` → view files → `AppShell.jsx`.
- `AppShell.jsx` — sidebar + topbar + content router + ⌘K palette + theme toggle.
- `DashboardView.jsx` — "Visão geral": metric cards + pipeline-by-stage + featured deals.
- `PipelineView.jsx` — funnel table (FunnelRow) with tabs + view switch.
- `ContactsView.jsx` — lead-card grid (LeadCard) with search + filters.
- `InboxView.jsx` — two-pane message inbox with composer.
- `MiscViews.jsx` — Campaigns table + generic empty state for other modules.
- `data.js` — all fake content on `window.FLUA_DATA`.

Each view registers itself on `window` (Babel scopes each `<script>` separately).

## Note
This is a visual/interaction recreation — actions are stubbed. It demonstrates component
coverage and the density/“all-day” feel, not production data flow.
