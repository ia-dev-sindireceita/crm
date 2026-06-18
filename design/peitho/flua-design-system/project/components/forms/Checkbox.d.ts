import * as React from "react";

export interface CheckboxProps {
  checked?: boolean;
  defaultChecked?: boolean;
  indeterminate?: boolean;
  onChange?: (next: boolean) => void;
  disabled?: boolean;
  label?: string;
  style?: React.CSSProperties;
}

/** Checkbox with accent fill and check / indeterminate states. */
export declare function Checkbox(props: CheckboxProps): JSX.Element;
