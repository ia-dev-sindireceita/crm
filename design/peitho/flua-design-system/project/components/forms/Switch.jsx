import React from "react";

/** Toggle switch — compact, accent-filled when on. */
export function Switch({ checked, defaultChecked, onChange, disabled, label, style }) {
  const [internal, setInternal] = React.useState(!!defaultChecked);
  const isOn = checked !== undefined ? checked : internal;
  const toggle = () => {
    if (disabled) return;
    if (checked === undefined) setInternal(!isOn);
    onChange && onChange(!isOn);
  };
  const sw = (
    <button
      type="button" role="switch" aria-checked={isOn} disabled={disabled} onClick={toggle}
      style={{
        position: "relative", width: 34, height: 20, flexShrink: 0, padding: 0,
        border: "none", borderRadius: "var(--radius-full)", cursor: disabled ? "not-allowed" : "pointer",
        background: isOn ? "var(--accent)" : "var(--gray-300)",
        opacity: disabled ? 0.5 : 1, transition: "background .16s ease",
      }}
    >
      <span style={{
        position: "absolute", top: 2, left: isOn ? 16 : 2, width: 16, height: 16,
        borderRadius: "50%", background: "#fff", boxShadow: "0 1px 2px rgba(0,0,0,.25)",
        transition: "left .16s cubic-bezier(.4,0,.2,1)",
      }} />
    </button>
  );
  if (!label) return <span style={style}>{sw}</span>;
  return (
    <label style={{ display: "inline-flex", alignItems: "center", gap: 9, cursor: disabled ? "not-allowed" : "pointer", ...style }}>
      {sw}
      <span style={{ fontSize: "var(--text-base)", color: "var(--text-primary)" }}>{label}</span>
    </label>
  );
}
