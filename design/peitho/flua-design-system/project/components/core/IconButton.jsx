import React from "react";
import { Icon } from "./Icon.jsx";

const SIZES = { sm: 28, md: 32, lg: 38 };
const ICON = { sm: 15, md: 16, lg: 18 };

/** Square icon-only button. Use for toolbar / row actions. */
export function IconButton({
  icon, size = "md", variant = "ghost", label, disabled = false, active = false, style, ...rest
}) {
  const px = SIZES[size] || SIZES.md;
  const [hover, setHover] = React.useState(false);

  const base = { background: "transparent", color: "var(--text-secondary)", boxShadow: "none" };
  const styles = {
    ghost: { ...base, hoverBg: "var(--bg-subtle)", hoverColor: "var(--text-primary)" },
    solid: {
      background: "var(--accent)", color: "#fff", boxShadow: "none",
      hoverBg: "var(--accent-hover)", hoverColor: "#fff",
    },
    outline: {
      background: "var(--surface)", color: "var(--text-secondary)",
      boxShadow: "inset 0 0 0 1px var(--border)",
      hoverBg: "var(--surface-2)", hoverColor: "var(--text-primary)",
    },
  }[variant] || base;

  const isActive = active;
  return (
    <button
      type="button"
      disabled={disabled}
      aria-label={label}
      title={label}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: "inline-flex", alignItems: "center", justifyContent: "center",
        width: px, height: px, padding: 0, border: "none",
        borderRadius: "var(--radius-md)", cursor: disabled ? "not-allowed" : "pointer",
        opacity: disabled ? 0.45 : 1,
        background: isActive ? "var(--accent-soft)" : (hover ? styles.hoverBg : styles.background),
        color: isActive ? "var(--accent)" : (hover ? styles.hoverColor : styles.color),
        boxShadow: styles.boxShadow,
        transition: "background .14s ease, color .14s ease",
        ...style,
      }}
      {...rest}
    >
      <Icon name={icon} size={ICON[size] || 16} />
    </button>
  );
}
