import React from "react";

/** Lightweight hover tooltip. Wraps a single trigger child. */
export function Tooltip({ label, side = "top", children, style }) {
  const [show, setShow] = React.useState(false);
  const pos = {
    top:    { bottom: "calc(100% + 6px)", left: "50%", transform: "translateX(-50%)" },
    bottom: { top: "calc(100% + 6px)", left: "50%", transform: "translateX(-50%)" },
    left:   { right: "calc(100% + 6px)", top: "50%", transform: "translateY(-50%)" },
    right:  { left: "calc(100% + 6px)", top: "50%", transform: "translateY(-50%)" },
  }[side];
  return (
    <span
      style={{ position: "relative", display: "inline-flex", ...style }}
      onMouseEnter={() => setShow(true)} onMouseLeave={() => setShow(false)}
    >
      {children}
      {show && (
        <span
          role="tooltip"
          style={{
            position: "absolute", zIndex: 50, ...pos, whiteSpace: "nowrap",
            padding: "5px 8px", fontSize: "var(--text-xs)", fontWeight: "var(--weight-medium)",
            color: "var(--gray-25)", background: "var(--gray-800)",
            borderRadius: "var(--radius-sm)", boxShadow: "var(--shadow-md)",
            pointerEvents: "none",
          }}
        >
          {label}
        </span>
      )}
    </span>
  );
}
