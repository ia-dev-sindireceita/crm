import * as React from "react";
import type { IconName } from "../core/Icon";

export interface TagProps {
  children: React.ReactNode;
  /** Show a colored dot before the label. */
  color?: string;
  /** Optional leading icon. */
  icon?: IconName;
  /** When provided, renders a remove (×) button. */
  onRemove?: () => void;
  style?: React.CSSProperties;
}

/** Subtle label/keyword chip, optionally removable. */
export declare function Tag(props: TagProps): JSX.Element;
