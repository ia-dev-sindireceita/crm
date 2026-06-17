import * as React from "react";
import type { IconName } from "./Icon";

export interface ButtonProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, "style"> {
  /** Visual emphasis. Default "primary". */
  variant?: "primary" | "secondary" | "ghost" | "danger";
  /** Control height. Default "md". */
  size?: "sm" | "md" | "lg";
  /** Leading icon name. */
  leftIcon?: IconName;
  /** Trailing icon name. */
  rightIcon?: IconName;
  /** Stretch to container width. */
  fullWidth?: boolean;
  disabled?: boolean;
  style?: React.CSSProperties;
}

/**
 * Primary action button for the Peitho UI.
 * @startingPoint section="Core" subtitle="Buttons in every variant & size" viewport="700x180"
 */
export declare function Button(props: ButtonProps): JSX.Element;
