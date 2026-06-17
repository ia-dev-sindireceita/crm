import React from "react";
import { Avatar } from "../data-display/Avatar.jsx";

/** Ordered pipeline stages with their dot color. */
export const PIPELINE_STAGES = [
  { id: "new",        label: "Novo",          color: "var(--blue-500)" },
  { id: "qualified",  label: "Qualificado",   color: "var(--teal-500)" },
  { id: "negotiating",label: "Negociação",    color: "var(--amber-500)" },
  { id: "won",        label: "Fechado",       color: "var(--green-500)" },
];

/**
 * Funnel table row with a segmented stage indicator.
 * Designed to sit inside a table-like flex column with a header row.
 */
export function FunnelRow({ deal, stages = PIPELINE_STAGES, onClick, style }) {
  const { name, company, value, owner, stageIndex = 0, status } = deal || {};
  const [hover, setHover] = React.useState(false);
  const activeColor = stages[Math.min(stageIndex, stages.length - 1)]?.color || "var(--accent)";

  return (
    <div
      onClick={onClick}
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: "grid", gridTemplateColumns: "1.6fr 1.4fr 0.9fr 36px",
        alignItems: "center", gap: 16, padding: "11px 16px",
        background: hover ? "var(--surface-2)" : "transparent",
        borderBottom: "1px solid var(--border-subtle)", cursor: "pointer",
        transition: "background .12s ease", ...style,
      }}
    >
      {/* Deal + company */}
      <div style={{ minWidth: 0 }}>
        <div style={{ fontSize: "var(--text-base)", fontWeight: "var(--weight-medium)",
          color: "var(--text-primary)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{name}</div>
        <div style={{ fontSize: "var(--text-xs)", color: "var(--text-tertiary)",
          whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{company}</div>
      </div>

      {/* Segmented stage indicator */}
      <div>
        <div style={{ display: "flex", gap: 3, marginBottom: 5 }}>
          {stages.map((st, i) => (
            <span key={st.id} style={{ flex: 1, height: 4, borderRadius: "var(--radius-full)",
              background: i <= stageIndex ? activeColor : "var(--border)",
              transition: "background .2s ease" }} />
          ))}
        </div>
        <span style={{ fontSize: "var(--text-2xs)", fontWeight: "var(--weight-medium)", color: activeColor }}>
          {stages[Math.min(stageIndex, stages.length - 1)]?.label}
        </span>
      </div>

      {/* Value */}
      <div className="tnum" style={{ fontSize: "var(--text-base)", fontWeight: "var(--weight-semibold)",
        color: "var(--text-primary)", textAlign: "right" }}>{value}</div>

      {/* Owner */}
      <div style={{ display: "flex", justifyContent: "center" }}>
        {owner && <Avatar name={owner} size="sm" />}
      </div>
    </div>
  );
}
