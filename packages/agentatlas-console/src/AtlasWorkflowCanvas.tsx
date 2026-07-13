// AtlasWorkflowCanvas wraps FlowGram's free-layout editor. Business code
// only ever sees AtlasWorkflow; FlowGram internals stay behind this file so
// the canvas dependency can be swapped without touching the object model.
import { useMemo, useRef } from "react";
import {
  FreeLayoutEditor,
  WorkflowNodeRenderer,
  useNodeRender,
  type FreeLayoutPluginContext,
  type WorkflowJSON,
} from "@flowgram.ai/free-layout-editor";
import { LegacyButton as ClawButton } from "./app/runtime-ui-adapters";
import type { AtlasWorkflow, AtlasWorkflowNode } from "./types";
export { toWorkflowJSON } from "./features/workflows/flowgram-adapter";
import { toWorkflowJSON } from "./features/workflows/flowgram-adapter";

const NODE_TYPES: string[] = [
  "input.manual", "input.evidence_pointer", "parser.document", "parser.image",
  "parser.long_image", "parser.audio", "parser.video", "transform.extract_sop",
  "transform.summarize", "retrieval.search", "nexus.locate", "nexus.read",
  "human.confirm", "dream.aggregate", "answer.generate", "trace.append",
];

const CATEGORY_COLOR: Record<string, string> = {
  input: "var(--claw-accent-soft)",
  parser: "var(--claw-warning-soft)",
  transform: "var(--claw-success-soft)",
  retrieval: "var(--claw-accent-soft)",
  nexus: "var(--claw-danger-soft)",
  human: "var(--claw-warning-soft)",
  dream: "var(--claw-accent-soft)",
  answer: "var(--claw-success-soft)",
  trace: "var(--claw-accent-soft)",
};

function AtlasNodeCard() {
  const { node, data } = useNodeRender();
  const atlasType = String(data?.atlasType ?? node.flowNodeType ?? "");
  const category = atlasType.split(".")[0] ?? "input";
  return (
    <WorkflowNodeRenderer node={node}>
      <div
        data-testid={`atlas-node-${node.id}`}
        style={{
          minWidth: 180,
          padding: "10px 14px",
          background: "var(--claw-surface-solid)",
          border: "1px solid var(--claw-border-strong)",
          borderRadius: "var(--claw-radius-md)",
          boxShadow: "var(--claw-shadow-1)",
          fontFamily: "var(--claw-font)",
        }}
      >
        <div
          style={{
            display: "inline-block",
            padding: "1px 8px",
            marginBottom: 6,
            fontSize: 11,
            borderRadius: 999,
            background: CATEGORY_COLOR[category] ?? "var(--claw-accent-soft)",
            color: "var(--claw-text-secondary)",
          }}
        >
          {atlasType}
        </div>
        <div style={{ fontSize: 14, fontWeight: 600, color: "var(--claw-text)" }}>
          {String(data?.title ?? node.id)}
        </div>
        {data?.requiresConfirmation ? (
          <div style={{ marginTop: 4, fontSize: 11, color: "var(--claw-warning)" }}>需人工确认</div>
        ) : null}
      </div>
    </WorkflowNodeRenderer>
  );
}

export interface AtlasWorkflowCanvasProps {
  workflow: AtlasWorkflow;
  readonly?: boolean;
  /** Receives the edited canvas document (nodes/edges JSON) on demand. */
  onExport?: (json: WorkflowJSON) => void;
}

export function AtlasWorkflowCanvas({ workflow, readonly, onExport }: AtlasWorkflowCanvasProps) {
  const ctxRef = useRef<FreeLayoutPluginContext | null>(null);
  const initialData = useMemo(() => toWorkflowJSON(workflow), [workflow]);
  const nodeRegistries = useMemo(() => [...new Set([...NODE_TYPES, ...workflow.nodes.map((node) => node.type)])].map((type) => ({ type })), [workflow.nodes]);
  const confirmCount = workflow.nodes.filter(
    (n: AtlasWorkflowNode) => n.type === "human.confirm" || n.requires_confirmation,
  ).length;

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%", minHeight: 420 }}>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 12,
          padding: "8px 12px",
          fontSize: 13,
          fontFamily: "var(--claw-font)",
          color: "var(--claw-text-secondary)",
          borderBottom: "1px solid var(--claw-border)",
        }}
      >
        <strong style={{ color: "var(--claw-text)" }}>{workflow.workflow_id}</strong>
        <span>kind: {workflow.kind}</span>
        <span data-testid="canvas-node-count">节点 {workflow.nodes.length}</span>
        <span>确认点 {confirmCount}</span>
        <span
          style={{
            padding: "1px 8px",
            borderRadius: 999,
            background:
              workflow.risk_level === "high"
                ? "var(--claw-danger-soft)"
                : workflow.risk_level === "medium"
                  ? "var(--claw-warning-soft)"
                  : "var(--claw-success-soft)",
          }}
        >
          风险 {workflow.risk_level}
        </span>
        <span style={{ flex: 1 }} />
        {onExport ? (
          <ClawButton
            size="sm"
            variant="ghost"
            onClick={() => {
              const doc = ctxRef.current?.document;
              if (doc) onExport(doc.toJSON());
            }}
          >
            导出草稿
          </ClawButton>
        ) : null}
      </div>
      <div style={{ flex: 1, position: "relative" }} data-testid="atlas-workflow-canvas">
        <FreeLayoutEditor
          ref={ctxRef}
          initialData={initialData}
          nodeRegistries={nodeRegistries}
          readonly={readonly}
          materials={{ renderDefaultNode: AtlasNodeCard }}
        />
      </div>
    </div>
  );
}
