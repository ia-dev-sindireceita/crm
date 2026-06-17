import * as React from "react";

export interface Person { name: string; src?: string; }

export interface AvatarGroupProps {
  people: Person[];
  size?: "xs" | "sm" | "md" | "lg";
  /** Max avatars before a +N chip. Default 4. */
  max?: number;
  style?: React.CSSProperties;
}

/** Overlapping avatar stack with overflow count. */
export declare function AvatarGroup(props: AvatarGroupProps): JSX.Element;
