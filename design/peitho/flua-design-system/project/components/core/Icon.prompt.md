Stroke line-icon (curated Lucide subset) that inherits color from `currentColor` — use anywhere the UI needs an icon.

```jsx
<Icon name="search" size={16} />
<button style={{ color: "var(--accent)" }}><Icon name="plus" /> New lead</button>
```

- `name` — one of the `IconName` union (search, plus, users, inbox, zap, etc.).
- `size` defaults to 16 (CRM density); bump to 18–20 for nav, 14 for inline.
- Color comes from the parent's `color`. Stroke scales with size, so icons stay crisp.
