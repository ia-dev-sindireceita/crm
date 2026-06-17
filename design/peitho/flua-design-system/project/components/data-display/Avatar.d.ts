import * as React from "react";

export interface AvatarProps {
  /** Full name — used for initials and hashed color. */
  name?: string;
  /** Image URL; falls back to initials when absent. */
  src?: string;
  size?: "xs" | "sm" | "md" | "lg";
  /** Presence dot. */
  status?: "online" | "busy" | "offline";
  style?: React.CSSProperties;
}

/** Round avatar with image or hashed-color initials + optional presence dot. */
export declare function Avatar(props: AvatarProps): JSX.Element;
