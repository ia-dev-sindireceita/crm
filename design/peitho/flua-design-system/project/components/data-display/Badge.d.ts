import * as React from "react";
import type { IconName } from "../core/Icon";

export interface BadgeProps {
  children: React.ReactNode;
  /** Color tone. Default "neutral". */
  tone?: "neutral" | "accent" | "won" | "lost" | "nego" | "info";
  /** Optional leading icon. */
  icon?: IconName;
  /** Show a leading status dot. */
  dot?: boolean;
  size?: "sm" | "md";
  style?: React.CSSProperties;
}

/** Soft-tinted status pill. */
export declare function Badge(props: BadgeProps): JSX.Element;
