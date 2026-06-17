import * as React from "react";

export interface SwitchProps {
  checked?: boolean;
  defaultChecked?: boolean;
  onChange?: (next: boolean) => void;
  disabled?: boolean;
  /** Optional trailing label. */
  label?: string;
  style?: React.CSSProperties;
}

/** Compact on/off toggle, accent-filled when on. */
export declare function Switch(props: SwitchProps): JSX.Element;
