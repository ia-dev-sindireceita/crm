Action button — primary for the main action on a view, secondary/ghost for everything else, danger for destructive.

```jsx
<Button variant="primary" leftIcon="plus">New lead</Button>
<Button variant="secondary" size="sm">Filter</Button>
<Button variant="ghost" rightIcon="chevron-down">Sort</Button>
```

Variants: `primary` (indigo) · `secondary` (bordered surface) · `ghost` (transparent) · `danger` (muted rose). Sizes `sm | md | lg`. `fullWidth`, `disabled`, `leftIcon`/`rightIcon` take an `IconName`. Hover lightens/darkens, press shrinks slightly — no heavy shadows.
