import React from "react";

/** Keyboard key cap — for shortcuts like ⌘K. */
export function Kbd({ children, style }) {
  return (
    <kbd
      style={{
        display: "inline-flex", alignItems: "center", justifyContent: "center",
        minWidth: 18, height: 18, padding: "0 5px",
        fontFamily: "var(--font-sans)", fontSize: "var(--text-2xs)",
        fontWeight: "var(--weight-medium)", lineHeight: 1,
        color: "var(--text-secondary)", background: "var(--surface)",
        border: "1px solid var(--border)", borderRadius: "var(--radius-xs)",
        boxShadow: "0 1px 0 var(--border)",
        ...style,
      }}
    >
      {children}
    </kbd>
  );
}
