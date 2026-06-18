import * as React from "react";

export interface KbdProps {
  children: React.ReactNode;
  style?: React.CSSProperties;
}

/** Keyboard key cap for displaying shortcuts (e.g. ⌘K). */
export declare function Kbd(props: KbdProps): JSX.Element;
