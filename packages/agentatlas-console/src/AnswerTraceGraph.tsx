// Read-only answer trace visualization (React Flow): question -> spaces ->
// evidence (with grants) -> answer. Renders only ids present in the trace.
import { useMemo } from "react";
import { ReactFlow, Background, type Node, type Edge } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { AnswerTraceView } from "./types";

export interface AnswerTraceGraphProps {
  trace: AnswerTraceView;
}

export function AnswerTraceGraph({ trace }: AnswerTraceGraphProps) {
  const { nodes, edges } = useMemo(() => {
    const nodes: Node[] = [];
    const edges: Edge[] = [];
    const style = {
      fontFamily: "var(--claw-font)",
      fontSize: 12,
      border: "1px solid var(--claw-border-strong)",
      borderRadius: 12,
      padding: "6px 10px",
      background: "var(--claw-surface-solid)",
    };

    nodes.push({
      id: "question",
      position: { x: 0, y: 160 },
      data: { label: `问题 · ${trace.sanitized_question_summary ?? trace.trace_id}` },
      style, draggable: false, connectable: false,
    });
    trace.space_ids.forEach((s, i) => {
      nodes.push({ id: `space:${s}`, position: { x: 260, y: i * 90 }, data: { label: `空间 ${s}` }, style, draggable: false, connectable: false });
      edges.push({ id: `q->${s}`, source: "question", target: `space:${s}` });
    });
    trace.evidence_pointer_ids.forEach((ev, i) => {
      const grant = trace.agentnexus_read_grant_ids[i];
      nodes.push({
        id: `ev:${ev}`,
        position: { x: 520, y: i * 90 },
        data: { label: grant ? `证据 ${ev} · grant ${grant}` : `证据 ${ev}` },
        style, draggable: false, connectable: false,
      });
      const from = trace.space_ids[0] ? `space:${trace.space_ids[0]}` : "question";
      edges.push({ id: `s->${ev}`, source: from, target: `ev:${ev}` });
    });
    nodes.push({
      id: "answer",
      position: { x: 780, y: 160 },
      data: { label: `回答 · ${trace.model_route ?? ""} · hash ${trace.answer_hash.slice(0, 12)}` },
      style, draggable: false, connectable: false,
    });
    const answerSources = trace.evidence_pointer_ids.length
      ? trace.evidence_pointer_ids.map((ev) => `ev:${ev}`)
      : trace.space_ids.map((s) => `space:${s}`);
    (answerSources.length ? answerSources : ["question"]).forEach((src) => {
      edges.push({ id: `${src}->answer`, source: src, target: "answer" });
    });
    return { nodes, edges };
  }, [trace]);

  return (
    <div style={{ height: "100%", minHeight: 420 }} data-testid="answer-trace-graph">
      <ReactFlow nodes={nodes} edges={edges} fitView nodesDraggable={false} nodesConnectable={false} proOptions={{ hideAttribution: true }}>
        <Background gap={24} />
      </ReactFlow>
    </div>
  );
}
