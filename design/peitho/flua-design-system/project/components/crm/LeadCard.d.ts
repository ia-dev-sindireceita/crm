import * as React from "react";
import type { DealStatus } from "../data-display/StatusBadge";

export interface Lead {
  name: string;
  company?: string;
  email?: string;
  status?: DealStatus;
  /** Pre-formatted value string, e.g. "R$ 24.500". */
  value?: string;
  /** Owner full name (for the avatar). */
  owner?: string;
  /** Human last-activity label, e.g. "há 2h". */
  lastActivity?: string;
  tags?: string[];
}

export interface LeadCardProps {
  lead: Lead;
  /** Fired by the quick-action buttons. */
  onAction?: (action: "call" | "mail" | "message", lead: Lead) => void;
  style?: React.CSSProperties;
}

/**
 * Contact / lead card with status, value, owner and quick actions.
 * @startingPoint section="CRM" subtitle="Contact / lead card" viewport="360x200"
 */
export declare function LeadCard(props: LeadCardProps): JSX.Element;
