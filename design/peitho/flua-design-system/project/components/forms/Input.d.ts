import * as React from "react";
import type { IconName } from "../core/Icon";

export interface InputProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, "size" | "style"> {
  size?: "sm" | "md" | "lg";
  /** Leading icon (e.g. "search"). */
  leftIcon?: IconName;
  /** Field label above the control. */
  label?: string;
  /** Helper text below the field. */
  hint?: string;
  /** Error message — turns border rose and replaces hint. */
  error?: string;
  style?: React.CSSProperties;
  containerStyle?: React.CSSProperties;
}

/** Single-line text input with focus ring and optional icon/label/hint. */
export declare function Input(props: InputProps): JSX.Element;
