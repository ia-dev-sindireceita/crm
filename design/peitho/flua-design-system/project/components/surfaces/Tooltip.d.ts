import * as React from "react";

export interface TooltipProps {
  /** Tooltip text. */
  label: string;
  side?: "top" | "bottom" | "left" | "right";
  children: React.ReactNode;
  style?: React.CSSProperties;
}

/** Hover tooltip wrapping a single trigger element. */
export declare function Tooltip(props: TooltipProps): JSX.Element;
