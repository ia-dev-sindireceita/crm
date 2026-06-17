import React from "react";
import { Icon } from "../core/Icon.jsx";

const TONES = {
  neutral: { fg: "var(--status-neutral-fg)", bg: "var(--status-neutral-bg)" },
  accent:  { fg: "var(--accent)", bg: "var(--accent-soft)" },
  won:     { fg: "var(--status-won-fg)", bg: "var(--status-won-bg)" },
  lost:    { fg: "var(--status-lost-fg)", bg: "var(--status-lost-bg)" },
  nego:    { fg: "var(--status-nego-fg)", bg: "var(--status-nego-bg)" },
  info:    { fg: "var(--status-info-fg)", bg: "var(--status-info-bg)" },
};

/** Small status pill. Soft tinted background, no border. */
export function Badge({ children, tone = "neutral", icon, dot = false, size = "md", style }) {
  const t = TONES[tone] || TONES.neutral;
  const sm = size === "sm";
  return (
    <span
      style={{
        display: "inline-flex", alignItems: "center", gap: sm ? 4 : 5,
        height: sm ? 18 : 20, padding: sm ? "0 6px" : "0 8px",
        fontSize: sm ? "var(--text-2xs)" : "var(--text-xs)",
        fontWeight: "var(--weight-medium)", lineHeight: 1, whiteSpace: "nowrap",
        color: t.fg, background: t.bg, borderRadius: "var(--radius-full)",
        ...style,
      }}
    >
      {dot && <span style={{ width: 6, height: 6, borderRadius: "50%", background: t.fg, flexShrink: 0 }} />}
      {icon && <Icon name={icon} size={sm ? 11 : 12} />}
      {children}
    </span>
  );
}
