import * as React from "react";
import type { IconName } from "../core/Icon";

export interface NavEntry {
  id: string;
  label: string;
  icon: IconName;
  /** Optional count badge (e.g. unread inbox). */
  badge?: number | string;
}

export interface NavSection {
  /** Uppercase group label (omit for the top group). */
  label?: string;
  items: NavEntry[];
}

export interface SidebarNavProps {
  /** Flat list of items (use this OR `sections`). */
  items?: NavEntry[];
  /** Grouped items with section labels. */
  sections?: NavSection[];
  /** Currently active item id. */
  active?: string;
  onSelect?: (id: string) => void;
  /** Collapsed (icon-only) state. */
  collapsed?: boolean;
  onToggle?: () => void;
  /** Optional footer node (e.g. user chip). */
  footer?: React.ReactNode;
  style?: React.CSSProperties;
}

/**
 * Collapsible CRM navigation sidebar with the Peitho brand mark.
 * @startingPoint section="CRM" subtitle="Collapsible app navigation" viewport="260x560"
 */
export declare function SidebarNav(props: SidebarNavProps): JSX.Element;
