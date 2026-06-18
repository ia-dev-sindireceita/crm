import React from "react";
import { Icon } from "../core/Icon.jsx";

const SIZES = {
  sm: { h: "var(--control-sm)", font: "var(--text-sm)" },
  md: { h: "var(--control-md)", font: "var(--text-base)" },
};

/** Native select wrapped to match Peitho inputs. */
export function Select({ size = "md", label, value, defaultValue, onChange, options = [], disabled, style, containerStyle }) {
  const s = SIZES[size] || SIZES.md;
  const [focus, setFocus] = React.useState(false);
  return (
    <label style={{ display: "block", ...containerStyle }}>
      {label && (
        <span style={{ display: "block", marginBottom: 6, fontSize: "var(--text-xs)",
          fontWeight: "var(--weight-medium)", color: "var(--text-secondary)" }}>{label}</span>
      )}
      <span style={{ position: "relative", display: "block" }}>
        <select
          value={value} defaultValue={defaultValue} disabled={disabled}
          onChange={(e) => onChange && onChange(e.target.value)}
          onFocus={() => setFocus(true)} onBlur={() => setFocus(false)}
          style={{
            appearance: "none", WebkitAppearance: "none", width: "100%",
            height: s.h, padding: "0 32px 0 12px",
            fontFamily: "var(--font-sans)", fontSize: s.font, color: "var(--text-primary)",
            background: disabled ? "var(--bg-subtle)" : "var(--surface)",
            border: "none", borderRadius: "var(--radius-md)", cursor: disabled ? "not-allowed" : "pointer",
            boxShadow: focus ? "inset 0 0 0 1px var(--accent), 0 0 0 3px var(--accent-soft)" : "inset 0 0 0 1px var(--border)",
            transition: "box-shadow .14s ease", outline: "none", ...style,
          }}
        >
          {options.map((o) => {
            const opt = typeof o === "string" ? { value: o, label: o } : o;
            return <option key={opt.value} value={opt.value}>{opt.label}</option>;
          })}
        </select>
        <span style={{ position: "absolute", right: 10, top: "50%", transform: "translateY(-50%)",
          pointerEvents: "none", color: "var(--text-tertiary)", display: "flex" }}>
          <Icon name="chevron-down" size={15} />
        </span>
      </span>
    </label>
  );
}
