import * as React from "react";

export interface PipelineStage { id: string; label: string; color: string; }

export interface Deal {
  name: string;
  company?: string;
  /** Pre-formatted value, e.g. "R$ 24.500". */
  value?: string;
  owner?: string;
  /** Zero-based index into the stages array. */
  stageIndex?: number;
}

export interface FunnelRowProps {
  deal: Deal;
  /** Override the default 4-stage pipeline. */
  stages?: PipelineStage[];
  onClick?: () => void;
  style?: React.CSSProperties;
}

/** A pipeline/funnel table row with a segmented stage indicator. */
export declare function FunnelRow(props: FunnelRowProps): JSX.Element;
export declare const PIPELINE_STAGES: PipelineStage[];
