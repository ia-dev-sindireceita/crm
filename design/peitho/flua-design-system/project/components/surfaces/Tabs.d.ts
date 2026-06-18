import * as React from "react";
import type { IconName } from "../core/Icon";

export interface TabItem {
  value: string;
  label: string;
  icon?: IconName;
  /** Optional count chip. */
  count?: number;
}

export interface TabsProps {
  items: TabItem[];
  value?: string;
  defaultValue?: string;
  onChange?: (value: string) => void;
  style?: React.CSSProperties;
}

/** Underline tab bar with optional icons and count chips. */
export declare function Tabs(props: TabsProps): JSX.Element;
