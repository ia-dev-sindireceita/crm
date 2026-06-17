import React from "react";
import { Icon } from "../core/Icon.jsx";

/** Checkbox with accent fill + check icon when selected. */
export function Checkbox({ checked, defaultChecked, onChange, disabled, label, indeterminate, style }) {
  const [internal, setInternal] = React.useState(!!defaultChecked);
  const isOn = checked !== undefined ? checked : internal;
  const toggle = () => {
    if (disabled) return;
    if (checked === undefined) setInternal(!isOn);
    onChange && onChange(!isOn);
  };
  const box = (
    <span
      onClick={toggle} role="checkbox" aria-checked={indeterminate ? "mixed" : isOn}
      style={{
        display: "inline-flex", alignItems: "center", justifyContent: "center",
        width: 17, height: 17, flexShrink: 0, borderRadius: "var(--radius-xs)",
        cursor: disabled ? "not-allowed" : "pointer", opacity: disabled ? 0.5 : 1,
        background: (isOn || indeterminate) ? "var(--accent)" : "var(--surface)",
        boxShadow: (isOn || indeterminate) ? "none" : "inset 0 0 0 1.5px var(--border-strong)",
        color: "#fff", transition: "background .12s ease, box-shadow .12s ease",
      }}
    >
      {indeterminate
        ? <span style={{ width: 8, height: 2, borderRadius: 1, background: "#fff" }} />
        : isOn && <Icon name="check" size={12} strokeWidth={3} />}
    </span>
  );
  if (!label) return <span style={style}>{box}</span>;
  return (
    <label style={{ display: "inline-flex", alignItems: "center", gap: 8, cursor: disabled ? "not-allowed" : "pointer", ...style }}>
      {box}
      <span style={{ fontSize: "var(--text-base)", color: "var(--text-primary)" }}>{label}</span>
    </label>
  );
}
