import { useMemo, useState } from "react";
import type { DreamRun } from "./api/dream";
import type { TimelineNode } from "./types";
import { DreamHeader, DreamSubnav } from "./features/dream/DreamOverviewPage";
import "./features/dream/dream.css";

export function DreamTimeline({ runs = [], organizations = [], nodes = [], state = "ready" }: { runs?: DreamRun[]; organizations?: Array<{ id: string; name: string }>; nodes?: TimelineNode[]; state?: "ready" | "loading" | "error" }) {
  const [org, setOrg] = useState(organizations[0]?.id ?? "");
  const [window, setWindow] = useState("all");
  const legacyRuns = useMemo(() => nodes.map(legacyNodeToRun), [nodes]);
  const allRuns = runs.length ? runs : legacyRuns;
  const filtered = useMemo(() => allRuns.filter((run) => (!org || run.org_unit_id === org) && withinWindow(run, window)), [allRuns, org, window]);
  const names = new Map(organizations.map((item) => [item.id, item.name]));
  return <main className="dream-page">
    <DreamHeader title="梦境时间线" description="按组织和时间查看每次整理出的摘要、趋势、风险、待办和确认状态。" />
    <DreamSubnav current="timeline" />
    {state !== "ready" ? <div className="dream-state" role={state === "error" ? "alert" : "status"}><strong>{state === "loading" ? "正在读取梦境时间线…" : "暂时无法读取梦境时间线"}</strong><span>{state === "loading" ? "下一步：请稍候，页面会自动显示最新结果。" : "下一步：检查网络后重试；历史结果没有被修改。"}</span></div> : <>
      <div className="glass-rest dream-toolbar"><label className="dream-field">选择组织<select aria-label="选择组织" value={org} onChange={(event) => setOrg(event.target.value)}>{organizations.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label className="dream-field">时间范围<select aria-label="时间范围" value={window} onChange={(event) => setWindow(event.target.value)}><option value="all">全部时间</option><option value="7">最近 7 天</option><option value="30">最近 30 天</option></select></label></div>
      {filtered.length ? <ol className="dream-timeline-list">{filtered.map((run) => <li key={run.run_id}><article className="glass-rest dream-timeline-card"><header><h2>{names.get(run.org_unit_id) ?? "授权组织"}</h2><time>{new Date(run.window_end).toLocaleString("zh-CN")}</time></header><p className="dream-summary">{run.display_summary}</p><span className="dream-status" data-state={run.missing_inputs.length ? "attention" : "ok"}>{confirmationLabel(run)}</span><div className="dream-signals"><SignalGroup title="趋势" items={run.trends} /><SignalGroup title="风险" items={run.risks} /><SignalGroup title="待办" items={run.todos} /></div></article></li>)}</ol> : <div className="dream-state"><strong>这个范围还没有整理结果</strong><span>下一步：切换组织或扩大时间范围。</span></div>}
    </>}
  </main>;
}

function legacyNodeToRun(node: TimelineNode): DreamRun { return { run_id: node.timeline_node_id, status: "succeeded", org_unit_id: "", window_start: node.node_time, window_end: node.node_time, policy_version: 1, workflow: { id: "legacy", version: 1 }, parent_run_ids: [], input_count: 1, coverage: { expected_children: 0, completed_children: 0, input_count: 1 }, missing_inputs: [], facts: [], themes: [], trends: [], risks: [], todos: [], display_summary: node.summary_text, evidence_pointer_id: "", input_snapshot: { source_counts: [], sanitized_input_ids: [] }, visibility_snapshot: { visibility_level: "managers", org_unit_ids: [], masked_field_count: 0 }, model_route: "legacy", model_version: "legacy", attempt: 1, idempotency_key: node.timeline_node_id }; }
function SignalGroup({ title, items }: { title: string; items: DreamRun["trends"] }) { return <section><h3>{title}</h3>{items.length ? <ul>{items.map((item) => <li key={item.id}>{item.title}</li>)}</ul> : <span className="dream-meta-label">暂无{title}</span>}</section>; }
function withinWindow(run: DreamRun, window: string) { return window === "all" || new Date(run.window_end).getTime() >= Date.now() - Number(window) * 86_400_000; }
function confirmationLabel(run: DreamRun) { if (run.status === "waiting_confirmation") return "等待负责人确认"; if (run.missing_inputs.length) return "等待补齐输入"; if (run.status === "failed") return "整理失败"; return "已完成"; }
