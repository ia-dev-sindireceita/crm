import React from "react";

/** Surface container. Border + soft shadow, no heavy elevation. */
export function Card({ children, padding = 16, interactive = false, style, ...rest }) {
  const [hover, setHover] = React.useState(false);
  return (
    <div
      onMouseEnter={() => interactive && setHover(true)}
      onMouseLeave={() => interactive && setHover(false)}
      style={{
        background: "var(--surface)",
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        boxShadow: hover ? "var(--shadow-md)" : "var(--shadow-sm)",
        padding,
        cursor: interactive ? "pointer" : "default",
        transition: "box-shadow .16s ease, border-color .16s ease, transform .12s ease",
        borderColor: hover ? "var(--border-strong)" : "var(--border)",
        ...style,
      }}
      {...rest}
    >
      {children}
    </div>
  );
}
