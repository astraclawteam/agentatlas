// The console shell: four work surfaces (knowledge map, dream timeline,
// workflow canvas, answer trace) plus the floating Atlas Agent launcher.
import { useState } from "react";
import { KnowledgeMap } from "./KnowledgeMap";
import { DreamTimeline } from "./DreamTimeline";
import { AtlasWorkflowCanvas } from "./AtlasWorkflowCanvas";
import { AnswerTraceGraph } from "./AnswerTraceGraph";
import { AtlasAgentLauncher } from "./AtlasAgentLauncher";
import type { AnswerTraceView, AtlasWorkflow, KnowledgeSpace, TimelineNode } from "./types";

const SURFACES = [
  { key: "map", label: "组织知识地图" },
  { key: "timeline", label: "梦境时间线" },
  { key: "workflow", label: "知识工作流" },
  { key: "trace", label: "证据追溯" },
] as const;

type SurfaceKey = (typeof SURFACES)[number]["key"];

export interface AgentAtlasDashboardProps {
  spaces: KnowledgeSpace[];
  spaceLinks?: Array<{ from: string; to: string }>;
  timeline: TimelineNode[];
  workflow: AtlasWorkflow | null;
  trace: AnswerTraceView | null;
  banner?: string;
}

export function AgentAtlasDashboard({ spaces, spaceLinks, timeline, workflow, trace, banner }: AgentAtlasDashboardProps) {
  const [surface, setSurface] = useState<SurfaceKey>("map");

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100vh", background: "var(--claw-bg)", fontFamily: "var(--claw-font)" }}>
      <header style={{ display: "flex", alignItems: "center", gap: 16, padding: "12px 20px", borderBottom: "1px solid var(--claw-border)" }}>
        <h1 style={{ margin: 0, fontSize: 18, color: "var(--claw-text)" }}>AgentAtlas Console</h1>
        <nav role="tablist" aria-label="工作面" style={{ display: "flex", gap: 4 }}>
          {SURFACES.map((s) => (
            <button
              key={s.key}
              role="tab"
              aria-selected={surface === s.key}
              onClick={() => setSurface(s.key)}
              style={{
                padding: "6px 14px",
                fontSize: 13,
                cursor: "pointer",
                border: "none",
                borderRadius: "var(--claw-radius-sm)",
                background: surface === s.key ? "var(--claw-accent-soft)" : "transparent",
                color: surface === s.key ? "var(--claw-accent)" : "var(--claw-text-secondary)",
                fontWeight: surface === s.key ? 600 : 400,
              }}
            >
              {s.label}
            </button>
          ))}
        </nav>
      </header>
      {banner ? (
        <div style={{ padding: "6px 20px", fontSize: 12, background: "var(--claw-warning-soft)", color: "var(--claw-text-secondary)" }}>
          {banner}
        </div>
      ) : null}
      <main style={{ flex: 1, overflow: "hidden" }}>
        {surface === "map" ? <KnowledgeMap spaces={spaces} links={spaceLinks} /> : null}
        {surface === "timeline" ? (
          <div style={{ height: "100%", overflowY: "auto" }}>
            <DreamTimeline nodes={timeline} />
          </div>
        ) : null}
        {surface === "workflow" ? (
          workflow ? (
            <AtlasWorkflowCanvas workflow={workflow} />
          ) : (
            <div style={{ padding: 32, color: "var(--claw-text-muted)" }}>暂无工作流。用 Atlas Agent 生成草稿。</div>
          )
        ) : null}
        {surface === "trace" ? (
          trace ? (
            <AnswerTraceGraph trace={trace} />
          ) : (
            <div style={{ padding: 32, color: "var(--claw-text-muted)" }}>暂无回答追溯记录。</div>
          )
        ) : null}
      </main>
      <AtlasAgentLauncher />
    </div>
  );
}
