import React from "react";
import { Icon } from "../core/Icon.jsx";

/** Underline tab bar. Controlled via `value` or uncontrolled via `defaultValue`. */
export function Tabs({ items = [], value, defaultValue, onChange, style }) {
  const [internal, setInternal] = React.useState(defaultValue ?? (items[0] && items[0].value));
  const active = value !== undefined ? value : internal;
  const select = (v) => {
    if (value === undefined) setInternal(v);
    onChange && onChange(v);
  };
  return (
    <div role="tablist" style={{ display: "flex", gap: 2, borderBottom: "1px solid var(--border)", ...style }}>
      {items.map((it) => {
        const on = it.value === active;
        return (
          <button
            key={it.value} role="tab" aria-selected={on} onClick={() => select(it.value)}
            style={{
              position: "relative", display: "inline-flex", alignItems: "center", gap: 6,
              height: 34, padding: "0 12px", border: "none", background: "transparent",
              fontFamily: "var(--font-sans)", fontSize: "var(--text-sm)",
              fontWeight: "var(--weight-medium)", cursor: "pointer",
              color: on ? "var(--text-primary)" : "var(--text-secondary)",
              transition: "color .14s ease",
            }}
          >
            {it.icon && <Icon name={it.icon} size={15} />}
            {it.label}
            {it.count != null && (
              <span style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)",
                background: "var(--bg-subtle)", borderRadius: "var(--radius-full)", padding: "1px 6px" }}>{it.count}</span>
            )}
            <span style={{ position: "absolute", left: 6, right: 6, bottom: -1, height: 2,
              borderRadius: "2px 2px 0 0", background: on ? "var(--accent)" : "transparent",
              transition: "background .14s ease" }} />
          </button>
        );
      })}
    </div>
  );
}
