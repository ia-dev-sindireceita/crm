import * as React from "react";

export type DealStatus = "won" | "lost" | "negotiating" | "new" | "qualified" | "open";

export interface StatusBadgeProps {
  /** Deal/pipeline stage. Maps to the correct tone + PT-BR label. */
  status?: DealStatus;
  /** Override the default label. */
  label?: string;
  size?: "sm" | "md";
  style?: React.CSSProperties;
}

/** Canonical deal-stage indicator (Ganho / Perdido / Em negociação …). */
export declare function StatusBadge(props: StatusBadgeProps): JSX.Element;
