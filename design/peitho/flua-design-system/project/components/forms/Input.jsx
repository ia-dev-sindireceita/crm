import React from "react";
import { Icon } from "../core/Icon.jsx";

const SIZES = {
  sm: { h: "var(--control-sm)", font: "var(--text-sm)", pad: 10 },
  md: { h: "var(--control-md)", font: "var(--text-base)", pad: 12 },
  lg: { h: "var(--control-lg)", font: "var(--text-md)", pad: 14 },
};

/** Text input with optional leading icon and inline label. */
export function Input({
  size = "md", leftIcon, label, hint, error, value, defaultValue,
  placeholder, type = "text", disabled, style, containerStyle, ...rest
}) {
  const s = SIZES[size] || SIZES.md;
  const [focus, setFocus] = React.useState(false);
  return (
    <label style={{ display: "block", ...containerStyle }}>
      {label && (
        <span style={{ display: "block", marginBottom: 6, fontSize: "var(--text-xs)",
          fontWeight: "var(--weight-medium)", color: "var(--text-secondary)" }}>{label}</span>
      )}
      <span
        style={{
          display: "flex", alignItems: "center", gap: 7,
          height: s.h, padding: `0 ${s.pad}px`,
          background: disabled ? "var(--bg-subtle)" : "var(--surface)",
          borderRadius: "var(--radius-md)",
          boxShadow: error ? "inset 0 0 0 1px var(--rose-500)"
            : focus ? "inset 0 0 0 1px var(--accent), 0 0 0 3px var(--accent-soft)"
            : "inset 0 0 0 1px var(--border)",
          transition: "box-shadow .14s ease",
        }}
      >
        {leftIcon && <Icon name={leftIcon} size={s === SIZES.sm ? 14 : 16} style={{ color: "var(--text-tertiary)" }} />}
        <input
          type={type} value={value} defaultValue={defaultValue}
          placeholder={placeholder} disabled={disabled}
          onFocus={() => setFocus(true)} onBlur={() => setFocus(false)}
          style={{
            flex: 1, minWidth: 0, height: "100%", border: "none", outline: "none",
            background: "transparent", color: "var(--text-primary)",
            fontFamily: "var(--font-sans)", fontSize: s.font, padding: 0, ...style,
          }}
          {...rest}
        />
      </span>
      {(hint || error) && (
        <span style={{ display: "block", marginTop: 5, fontSize: "var(--text-xs)",
          color: error ? "var(--rose-500)" : "var(--text-tertiary)" }}>{error || hint}</span>
      )}
    </label>
  );
}
