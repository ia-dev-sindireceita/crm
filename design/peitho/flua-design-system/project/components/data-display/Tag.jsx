import React from "react";
import { Icon } from "../core/Icon.jsx";

/** Removable tag / label chip. Neutral by default. */
export function Tag({ children, onRemove, icon, color, style }) {
  return (
    <span
      style={{
        display: "inline-flex", alignItems: "center", gap: 5,
        height: 22, padding: onRemove ? "0 5px 0 8px" : "0 9px",
        fontSize: "var(--text-xs)", fontWeight: "var(--weight-medium)",
        color: "var(--text-secondary)", background: "var(--bg-subtle)",
        border: "1px solid var(--border-subtle)", borderRadius: "var(--radius-sm)",
        lineHeight: 1, ...style,
      }}
    >
      {color && <span style={{ width: 7, height: 7, borderRadius: "50%", background: color, flexShrink: 0 }} />}
      {icon && <Icon name={icon} size={12} />}
      {children}
      {onRemove && (
        <button
          type="button"
          onClick={onRemove}
          aria-label="Remover"
          style={{
            display: "inline-flex", alignItems: "center", justifyContent: "center",
            width: 16, height: 16, marginLeft: 1, padding: 0, border: "none",
            background: "transparent", color: "var(--text-tertiary)",
            borderRadius: "var(--radius-xs)", cursor: "pointer",
          }}
        >
          <Icon name="x" size={11} />
        </button>
      )}
    </span>
  );
}
