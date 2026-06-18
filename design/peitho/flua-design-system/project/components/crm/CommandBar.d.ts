import * as React from "react";
import type { IconName } from "../core/Icon";

export interface CommandItem {
  label: string;
  icon?: IconName;
  /** Right-aligned hint text. */
  hint?: string;
  /** Key caps shown on the right, e.g. ["⌘","N"]. */
  shortcut?: string[];
  onRun?: () => void;
}

export interface CommandGroup {
  label?: string;
  items: CommandItem[];
}

export interface CommandBarProps {
  open: boolean;
  onClose?: () => void;
  groups: CommandGroup[];
  placeholder?: string;
  style?: React.CSSProperties;
}

/**
 * ⌘K command palette with grouped, filterable commands and keyboard nav.
 * @startingPoint section="CRM" subtitle="⌘K command palette" viewport="600x420"
 */
export declare function CommandBar(props: CommandBarProps): JSX.Element | null;
