// Dream timeline: newest-first nodes with source badges and evidence chips.
import type { TimelineNode } from "./types";

const SOURCE_LABEL: Record<TimelineNode["source_type"], string> = {
  work_brief: "工作简报",
  dream_summary: "梦境汇总",
  sop_update: "SOP 更新",
  project_event: "项目事件",
  external_evidence: "外部证据",
  agent_answer: "Agent 回答",
};

export interface DreamTimelineProps {
  nodes: TimelineNode[];
  onEvidenceClick?: (evidencePointerID: string) => void;
}

export function DreamTimeline({ nodes, onEvidenceClick }: DreamTimelineProps) {
  if (nodes.length === 0) {
    return (
      <div style={{ padding: 32, fontFamily: "var(--claw-font)", color: "var(--claw-text-muted)" }}>
        暂无时间线节点。工作简报摄入与梦境任务运行后出现。
      </div>
    );
  }
  return (
    <ol
      data-testid="dream-timeline"
      style={{ listStyle: "none", margin: 0, padding: 16, display: "flex", flexDirection: "column", gap: 12, fontFamily: "var(--claw-font)" }}
    >
      {nodes.map((n) => (
        <li
          key={n.timeline_node_id}
          className="claw-glass"
          style={{ padding: 14, borderRadius: "var(--claw-radius-md)" }}
        >
          <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 6 }}>
            <span
              style={{
                fontSize: 11,
                padding: "1px 8px",
                borderRadius: 999,
                background: "var(--claw-accent-soft)",
                color: "var(--claw-text-secondary)",
              }}
            >
              {SOURCE_LABEL[n.source_type]}
            </span>
            <time style={{ fontSize: 12, color: "var(--claw-text-muted)" }}>
              {new Date(n.node_time).toLocaleString("zh-CN")}
            </time>
          </div>
          <div style={{ fontSize: 14, color: "var(--claw-text)" }}>{n.summary_text}</div>
          <div style={{ display: "flex", gap: 6, marginTop: 8, flexWrap: "wrap" }}>
            {(n.tags ?? []).map((t) => (
              <span key={t} style={{ fontSize: 11, color: "var(--claw-text-secondary)" }}>
                #{t}
              </span>
            ))}
            {n.evidence_pointer_id ? (
              <button
                onClick={() => onEvidenceClick?.(n.evidence_pointer_id!)}
                style={{
                  fontSize: 11,
                  border: "1px solid var(--claw-border-strong)",
                  background: "transparent",
                  borderRadius: 999,
                  padding: "1px 8px",
                  cursor: "pointer",
                  color: "var(--claw-accent)",
                }}
              >
                证据 {n.evidence_pointer_id}
              </button>
            ) : null}
          </div>
        </li>
      ))}
    </ol>
  );
}
