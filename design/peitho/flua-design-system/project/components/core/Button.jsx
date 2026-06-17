import React from "react";
import { Icon } from "./Icon.jsx";

const SIZES = {
  sm: { height: "var(--control-sm)", padding: "0 10px", font: "var(--text-sm)", gap: "5px", icon: 14 },
  md: { height: "var(--control-md)", padding: "0 14px", font: "var(--text-base)", gap: "6px", icon: 16 },
  lg: { height: "var(--control-lg)", padding: "0 18px", font: "var(--text-md)", gap: "8px", icon: 18 },
};

function variantStyle(variant) {
  switch (variant) {
    case "secondary":
      return {
        background: "var(--surface)", color: "var(--text-primary)",
        boxShadow: "inset 0 0 0 1px var(--border)",
        "--hover-bg": "var(--surface-2)", "--hover-shadow": "inset 0 0 0 1px var(--border-strong)",
      };
    case "ghost":
      return {
        background: "transparent", color: "var(--text-secondary)",
        "--hover-bg": "var(--bg-subtle)", "--hover-shadow": "none", "--hover-color": "var(--text-primary)",
      };
    case "danger":
      return {
        background: "var(--rose-500)", color: "#fff",
        "--hover-bg": "var(--rose-600)", "--hover-shadow": "none",
      };
    case "primary":
    default:
      return {
        background: "var(--accent)", color: "var(--text-on-accent)",
        "--hover-bg": "var(--accent-hover)", "--hover-shadow": "none",
      };
  }
}

/** Primary action control. Variants, sizes, optional leading/trailing icons. */
export function Button({
  children, variant = "primary", size = "md", leftIcon, rightIcon,
  disabled = false, fullWidth = false, type = "button", style, ...rest
}) {
  const s = SIZES[size] || SIZES.md;
  const v = variantStyle(variant);
  const [hover, setHover] = React.useState(false);
  const [active, setActive] = React.useState(false);

  return (
    <button
      type={type}
      disabled={disabled}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => { setHover(false); setActive(false); }}
      onMouseDown={() => setActive(true)}
      onMouseUp={() => setActive(false)}
      style={{
        display: fullWidth ? "flex" : "inline-flex",
        width: fullWidth ? "100%" : undefined,
        alignItems: "center", justifyContent: "center", gap: s.gap,
        height: s.height, padding: s.padding,
        fontFamily: "var(--font-sans)", fontSize: s.font, fontWeight: "var(--weight-medium)",
        lineHeight: 1, whiteSpace: "nowrap",
        border: "none", borderRadius: "var(--radius-md)",
        cursor: disabled ? "not-allowed" : "pointer",
        opacity: disabled ? 0.5 : 1,
        background: hover && !disabled && v["--hover-bg"] ? v["--hover-bg"] : v.background,
        color: hover && !disabled && v["--hover-color"] ? v["--hover-color"] : v.color,
        boxShadow: hover && !disabled && v["--hover-shadow"] !== undefined ? v["--hover-shadow"] : v.boxShadow,
        transform: active && !disabled ? "translateY(0.5px) scale(0.985)" : "none",
        transition: "background .14s ease, box-shadow .14s ease, color .14s ease, transform .06s ease",
        ...style,
      }}
      {...rest}
    >
      {leftIcon && <Icon name={leftIcon} size={s.icon} />}
      {children}
      {rightIcon && <Icon name={rightIcon} size={s.icon} />}
    </button>
  );
}
