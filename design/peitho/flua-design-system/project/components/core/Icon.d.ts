import * as React from "react";

export type IconName =
  | "search" | "plus" | "x" | "check" | "chevron-right" | "chevron-left"
  | "chevron-down" | "chevrons-left" | "more-horizontal" | "more-vertical"
  | "settings" | "users" | "user" | "inbox" | "megaphone" | "package"
  | "credit-card" | "bar-chart" | "trending-up" | "zap" | "sparkles" | "bell"
  | "phone" | "mail" | "message-circle" | "calendar" | "clock" | "panel-left"
  | "layout-dashboard" | "git-branch" | "filter" | "trash" | "edit" | "building"
  | "dollar-sign" | "arrow-up-right" | "star" | "circle" | "check-circle"
  | "sun" | "moon";

export interface IconProps extends React.SVGProps<SVGSVGElement> {
  /** Icon name from the Peitho (Lucide subset) set. */
  name: IconName;
  /** Pixel size of the square icon. Default 16. */
  size?: number;
  /** Stroke width in 24-unit viewBox space. Default 2. */
  strokeWidth?: number;
}

/** A stroke-based line icon that inherits color via currentColor. */
export declare function Icon(props: IconProps): JSX.Element | null;

export declare const ICON_PATHS: Record<IconName, string>;
