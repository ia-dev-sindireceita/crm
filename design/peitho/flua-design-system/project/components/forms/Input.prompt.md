Form controls — all reference the same tokens, 32px default height (density-medium).

```jsx
<Input label="E-mail" leftIcon="mail" placeholder="voce@empresa.com" />
<Select label="Estágio" options={["Novo","Qualificado","Em negociação"]} />
<Checkbox label="Receber alertas" defaultChecked />
<Switch label="Modo escuro" />
```

Focus shows an indigo ring (`--accent` border + `--accent-soft` glow). `Input` takes `error` to flag invalid state. `Checkbox`/`Switch` are controlled via `checked` or uncontrolled via `defaultChecked`.
