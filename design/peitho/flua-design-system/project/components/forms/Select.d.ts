import * as React from "react";

export interface SelectOption { value: string; label: string; }

export interface SelectProps {
  size?: "sm" | "md";
  label?: string;
  value?: string;
  defaultValue?: string;
  onChange?: (value: string) => void;
  /** Options as strings or {value,label}. */
  options: (string | SelectOption)[];
  disabled?: boolean;
  style?: React.CSSProperties;
  containerStyle?: React.CSSProperties;
}

/** Styled native select matching Peitho inputs. */
export declare function Select(props: SelectProps): JSX.Element;
