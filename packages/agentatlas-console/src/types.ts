// Frontend mirrors of the public contracts (schemas/workflow + atlas-runtime).

export type AtlasNodeType =
  | "input.manual"
  | "input.evidence_pointer"
  | "parser.document"
  | "parser.image"
  | "parser.long_image"
  | "parser.audio"
  | "parser.video"
  | "transform.extract_sop"
  | "transform.summarize"
  | "retrieval.search"
  | "nexus.locate"
  | "nexus.read"
  | "human.confirm"
  | "dream.aggregate"
  | "answer.generate"
  | "trace.append";

export interface AtlasWorkflowNode {
  id: string;
  type: AtlasNodeType;
  name?: string;
  requires_confirmation?: boolean;
  config?: Record<string, unknown>;
}

export interface AtlasWorkflowEdge {
  from: string;
  to: string;
  condition?: string;
}

export interface AtlasWorkflow {
  workflow_id: string;
  version: number;
  kind: "sop" | "dream" | "ingestion" | "answer";
  nodes: AtlasWorkflowNode[];
  edges: AtlasWorkflowEdge[];
  risk_level: "low" | "medium" | "high";
}

export interface KnowledgeSpace {
  space_id: string;
  enterprise_id: string;
  kind: "employee" | "project_group" | "department" | "business_unit" | "company";
  name: string;
  org_scope: string;
  org_version: number;
}

export interface TimelineNode {
  timeline_node_id: string;
  space_id: string;
  node_time: string;
  source_type: "work_brief" | "dream_summary" | "sop_update" | "project_event" | "external_evidence" | "agent_answer";
  summary_text: string;
  tags?: string[];
  evidence_pointer_id?: string;
}

export interface AnswerTraceView {
  trace_id: string;
  sanitized_question_summary?: string;
  space_ids: string[];
  evidence_pointer_ids: string[];
  agentnexus_read_grant_ids: string[];
  model_route?: string;
  answer_hash: string;
}
