import * as React from "react";

export interface CardProps extends Omit<React.HTMLAttributes<HTMLDivElement>, "style"> {
  children: React.ReactNode;
  /** Inner padding in px. Default 16. */
  padding?: number;
  /** Lift shadow + border on hover (use for clickable cards). */
  interactive?: boolean;
  style?: React.CSSProperties;
}

/**
 * Panel/card surface with border + soft shadow.
 * @startingPoint section="Surfaces" subtitle="Card container" viewport="700x200"
 */
export declare function Card(props: CardProps): JSX.Element;
