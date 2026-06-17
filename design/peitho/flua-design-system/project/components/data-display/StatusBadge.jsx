import React from "react";
import { Badge } from "./Badge.jsx";

/** Maps CRM deal stages to a Badge tone + label. The canonical won/lost/negotiating indicator. */
const STAGE = {
  won:        { tone: "won",  label: "Ganho",         dot: true },
  lost:       { tone: "lost", label: "Perdido",       dot: true },
  negotiating:{ tone: "nego", label: "Em negociação", dot: true },
  new:        { tone: "info", label: "Novo",          dot: true },
  qualified:  { tone: "accent", label: "Qualificado", dot: true },
  open:       { tone: "neutral", label: "Aberto",     dot: true },
};

export function StatusBadge({ status = "open", label, size = "md", style }) {
  const cfg = STAGE[status] || STAGE.open;
  return (
    <Badge tone={cfg.tone} dot={cfg.dot} size={size} style={style}>
      {label || cfg.label}
    </Badge>
  );
}
