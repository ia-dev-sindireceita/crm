import * as React from "react";
import type { IconName } from "./Icon";

export interface IconButtonProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, "style"> {
  /** Icon to render. */
  icon: IconName;
  /** Accessible label (also the tooltip title). */
  label?: string;
  size?: "sm" | "md" | "lg";
  /** "ghost" (default), "outline", or "solid" (accent). */
  variant?: "ghost" | "outline" | "solid";
  /** Persistent active/selected state (accent-soft bg). */
  active?: boolean;
  disabled?: boolean;
  style?: React.CSSProperties;
}

/** Square icon-only button for toolbars and row actions. */
export declare function IconButton(props: IconButtonProps): JSX.Element;
