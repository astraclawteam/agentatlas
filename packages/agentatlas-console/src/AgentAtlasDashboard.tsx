// Console 壳：常驻 Atlas Agent 对话（主入口）+ 五个证据视图。
// 数据全部来自真实后端（atlas-api / atlas-agent），票据在头部粘贴一次即可。
import { useCallback, useEffect, useState } from "react";
import { ClawButton, ClawInput } from "@agentatlas/claw-runtime-ui";
import { KnowledgeMap } from "./KnowledgeMap";
import { DreamTimeline } from "./DreamTimeline";
import { WorkflowStudio } from "./WorkflowStudio";
import { AnswerTraceGraph } from "./AnswerTraceGraph";
import { AtlasAgentPanel } from "./AtlasAgentPanel";
import { DreamPolicyPanel } from "./DreamPolicyPanel";
import { getTicket, listRecentTraces, listSpaces, listTimeline, setTicket } from "./api";
import type { AnswerTraceView, KnowledgeSpace, TimelineNode } from "./types";

const SURFACES = [
  { key: "map", label: "组织知识地图", desc: "企业的知识空间总览：公司/事业部/部门/项目组/员工，随组织架构自动生成，不用手建。" },
  { key: "timeline", label: "梦境时间线", desc: "系统每天自动写下的“工作日记”：员工简报与定期汇总按时间排列，点开能追到证据。" },
  { key: "policies", label: "梦境策略", desc: "告诉系统“每晚几点、汇总谁、哪些字段要打码”。策略发布后自动按时运行。" },
  { key: "workflow", label: "知识工作流", desc: "资料如何被解析、SOP 如何执行的“流水线图”。可以让 Agent 生成，也可以在这里手工加减步骤。" },
  { key: "trace", label: "证据追溯", desc: "每一条回答的“出处清单”：用了哪些空间、哪些证据、哪次授权。审计从这里开始。" },
] as const;

type SurfaceKey = (typeof SURFACES)[number]["key"];

export function AgentAtlasDashboard() {
  const [surface, setSurface] = useState<SurfaceKey>("map");
  const [ticket, setTicketState] = useState(getTicket());
  const [spaces, setSpaces] = useState<KnowledgeSpace[]>([]);
  const [timeline, setTimeline] = useState<TimelineNode[]>([]);
  const [traces, setTraces] = useState<AnswerTraceView[]>([]);
  const [selectedTrace, setSelectedTrace] = useState<AnswerTraceView | null>(null);
  const [loadError, setLoadError] = useState("");

  const refresh = useCallback(async () => {
    if (!getTicket()) {
      setSpaces([]);
      setTimeline([]);
      setTraces([]);
      return;
    }
    setLoadError("");
    try {
      const { spaces: sp } = await listSpaces();
      setSpaces(sp);
      // 时间线：聚合前几个空间的节点（新→旧）
      const nodes: TimelineNode[] = [];
      for (const s of sp.slice(0, 6)) {
        try {
          const { nodes: n } = await listTimeline(s.space_id);
          nodes.push(...n);
        } catch {
          /* 单空间失败不拖垮整页 */
        }
      }
      nodes.sort((a, b) => (b.node_time ?? "").localeCompare(a.node_time ?? ""));
      setTimeline(nodes);
      const { traces: tr } = await listRecentTraces();
      setTraces(tr);
      setSelectedTrace(tr[0] ?? null);
    } catch (e) {
      setLoadError(`加载失败：${(e as Error).message}`);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh, ticket]);

  const applyTicket = (v: string) => {
    setTicket(v);
    setTicketState(v);
  };

  const active = SURFACES.find((s) => s.key === surface)!;

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100vh", background: "var(--claw-bg)", fontFamily: "var(--claw-font)" }}>
      <header
        style={{
          display: "flex", alignItems: "center", gap: 16, padding: "12px 20px",
          borderBottom: "1px solid var(--claw-border)",
          background: "var(--claw-surface)",
          backdropFilter: "blur(var(--claw-glass-blur))",
          WebkitBackdropFilter: "blur(var(--claw-glass-blur))",
        }}
      >
        <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600, color: "var(--claw-text)" }}>AgentAtlas Console</h1>
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
                fontFamily: "var(--claw-font)",
                border: "none",
                borderRadius: "var(--claw-radius-sm)",
                // active = 真身 chip 渐变（陶土淡渐变底 + clay 字）
                background: surface === s.key ? "var(--claw-chip-bg)" : "transparent",
                color: surface === s.key ? "var(--claw-accent)" : "var(--claw-text-secondary)",
                fontWeight: surface === s.key ? 600 : 400,
              }}
            >
              {s.label}
            </button>
          ))}
        </nav>
        <span style={{ flex: 1 }} />
        <ClawInput
          aria-label="管理员票据"
          placeholder="粘贴管理员票据 tick_…"
          value={ticket}
          onChange={(e) => applyTicket(e.target.value)}
          style={{ width: 220 }}
        />
        <ClawButton size="sm" variant="ghost" onClick={() => void refresh()} disabled={!ticket}>
          刷新
        </ClawButton>
      </header>

      {/* 引导行走安静音量：muted 字 + 陶土左标（不再整条 accent 底当高音量横幅） */}
      <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "7px 20px", fontSize: 12, color: "var(--claw-text-muted)", borderBottom: "1px solid var(--claw-border)" }}>
        <span aria-hidden style={{ width: 3, height: 14, borderRadius: 2, background: "var(--claw-accent)", flexShrink: 0 }} />
        {active.desc}
      </div>
      {loadError ? (
        <div role="alert" style={{ padding: "6px 20px", fontSize: 12, background: "var(--claw-danger-soft)", color: "var(--claw-danger)" }}>
          {loadError}
        </div>
      ) : null}

      <div style={{ flex: 1, display: "flex", minHeight: 0 }}>
        <main style={{ flex: 1, minWidth: 0, overflow: "hidden" }}>
          {!ticket ? (
            // 欢迎屏 = 页面级玻璃卡（DESIGN §5 高光时刻，克制使用）
            <div style={{ display: "flex", justifyContent: "center", paddingTop: 64 }}>
              <div className="claw-glass" style={{ padding: "28px 32px", maxWidth: 560, color: "var(--claw-text-secondary)", fontSize: 14, lineHeight: 2 }}>
                <div style={{ fontSize: 20, fontWeight: 600, color: "var(--claw-text)", marginBottom: 6 }}>
                  欢迎使用 <em style={{ fontStyle: "normal", color: "var(--claw-accent)" }}>AgentAtlas</em>
                </div>
                这里是企业 Agent 的知识中枢。日常三件事：
                <br />1. 右侧对 Atlas Agent 说话（导入资料点 📎，说“做成 SOP”就生成流程）；
                <br />2. 员工在小智 Claw 里提问，这里能看到每条回答的证据；
                <br />3. 上方五个视图随时查看系统自动整理的结果。
                <br />
                <strong style={{ color: "var(--claw-text)" }}>第一步：在右上角粘贴管理员票据。</strong>
              </div>
            </div>
          ) : (
            <>
              {surface === "map" ? (
                spaces.length > 0 ? (
                  <KnowledgeMap spaces={spaces} links={[]} />
                ) : (
                  <EmptyHint text="还没有知识空间。接入 AgentNexus 组织架构后会自动出现（无需手建）。" />
                )
              ) : null}
              {surface === "timeline" ? (
                timeline.length > 0 ? (
                  <div style={{ height: "100%", overflowY: "auto" }}>
                    <DreamTimeline nodes={timeline} />
                  </div>
                ) : (
                  <EmptyHint text="时间线还是空的。员工简报入库、或梦境策略跑过一晚之后，这里会自动长出内容。" />
                )
              ) : null}
              {surface === "policies" ? <DreamPolicyPanel /> : null}
              {surface === "workflow" ? <WorkflowStudio /> : null}
              {surface === "trace" ? (
                traces.length > 0 ? (
                  <div style={{ display: "flex", height: "100%", minHeight: 0 }}>
                    <ul style={{ width: 280, flexShrink: 0, margin: 0, padding: 12, listStyle: "none", overflowY: "auto", borderRight: "1px solid var(--claw-border)", display: "flex", flexDirection: "column", gap: 8 }}>
                      {traces.map((t) => (
                        <li key={t.trace_id}>
                          <button
                            type="button"
                            onClick={() => setSelectedTrace(t)}
                            style={{
                              width: "100%", textAlign: "left", padding: "8px 10px", cursor: "pointer",
                              fontSize: 12, fontFamily: "var(--claw-font)",
                              border: "1px solid var(--claw-border)",
                              // 列表行强调态 = 陶土左条 + chip 渐变（DESIGN §5）
                              borderLeft: selectedTrace?.trace_id === t.trace_id ? "2px solid var(--claw-accent)" : "1px solid var(--claw-border)",
                              borderRadius: "var(--claw-radius-sm)",
                              background: selectedTrace?.trace_id === t.trace_id ? "var(--claw-chip-bg)" : "var(--claw-surface-solid)",
                              color: selectedTrace?.trace_id === t.trace_id ? "var(--claw-text)" : "var(--claw-text-secondary)",
                            }}
                          >
                            {t.sanitized_question_summary || t.trace_id}
                          </button>
                        </li>
                      ))}
                    </ul>
                    <div style={{ flex: 1, minWidth: 0 }}>
                      {selectedTrace ? <AnswerTraceGraph trace={selectedTrace} /> : null}
                    </div>
                  </div>
                ) : (
                  <EmptyHint text="还没有回答记录。在小智 Claw 里问一句（例如“我的工作内容是什么”），这里就会出现那条回答的完整证据链。" />
                )
              ) : null}
            </>
          )}
        </main>
        <AtlasAgentPanel />
      </div>
    </div>
  );
}

function EmptyHint({ text }: { text: string }) {
  return <div style={{ padding: 32, color: "var(--claw-text-muted)", fontSize: 13, lineHeight: 1.8 }}>{text}</div>;
}
