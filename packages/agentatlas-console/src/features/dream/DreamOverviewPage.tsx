import { AlertTriangle, CheckCircle2, Clock3, MoonStar } from "lucide-react";
import { Link, useInRouterContext } from "react-router-dom";
import type { DreamHierarchyNode, DreamRun } from "../../api/dream";
import "./dream.css";

export type DreamPageState = "ready" | "loading" | "error" | "empty";

export function DreamOverviewPage({ data = [], state = data.length ? "ready" : "empty" }: { data?: DreamHierarchyNode[]; state?: DreamPageState }) {
  return <main className="dream-page">
    <DreamHeader title="梦境全景" description="按组织逐级查看企业运行记录如何被整理成可追溯的组织记忆。" />
    <DreamSubnav current="overview" />
    {state !== "ready" ? <OverviewState state={state} /> : <ul className="dream-tree" aria-label="组织梦境层级">{data.map((node) => <TreeNode key={node.org.id} node={node} />)}</ul>}
  </main>;
}

function TreeNode({ node }: { node: DreamHierarchyNode }) {
  const run = node.run;
  const coverage = run?.coverage;
  const missing = run?.missing_input_reasons.length ?? 0;
  const attention = !run || run.status === "failed" || missing > 0 || (run.risks?.length ?? 0) > 0;
  return <li>
    <article className="glass-rest dream-org-card">
      <div className="dream-org-name"><strong>{node.org.name}</strong><small>{run ? humanWindow(run) : "还没有整理记录"}</small>{run ? <DreamNavLink to={`/dream/runs/${encodeURIComponent(run.handle)}`} current={false}>查看运行详情</DreamNavLink> : null}</div>
      <Metric label="最近一次">{run ? freshness(run.window_end) : "尚未运行"}</Metric>
      <Metric label="覆盖情况">{coverage?.expected_children ? `覆盖 ${coverage.completed_children}/${coverage.expected_children} 个下级组织` : "本层记录"}</Metric>
      <Metric label="缺失输入">{missing ? `有 ${missing} 项输入未完成` : "输入完整"}</Metric>
      <Metric label="风险提醒"><span className={run?.risks.length ? "dream-risk" : ""}>{run ? `${run.risks.length} 项` : "—"}</span></Metric>
      <div className="dream-org-metric"><span className="dream-meta-label">当前状态</span><span className="dream-status" data-state={attention ? "attention" : "ok"}>{statusLabel(run)}</span></div>
    </article>
    {node.children.length ? <ul>{node.children.map((child) => <TreeNode key={child.org.id} node={child} />)}</ul> : null}
  </li>;
}

function Metric({ label, children }: { label: string; children: React.ReactNode }) { return <div className="dream-org-metric"><span className="dream-meta-label">{label}</span><span>{children}</span></div>; }
function statusLabel(run?: DreamRun) { if (!run) return "等待首次整理"; if (run.status === "failed") return "整理失败"; if (run.status === "waiting_confirmation") return "等待确认"; if (run.missing_input_reasons.length || run.risks.length) return "需要留意"; if (run.status === "succeeded") return "结果完整"; return "正在整理"; }
function humanWindow(run: DreamRun) { return `${new Date(run.window_start).toLocaleDateString("zh-CN")} 至 ${new Date(run.window_end).toLocaleDateString("zh-CN")}`; }
function freshness(value: string) { const days = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 86_400_000)); return days === 0 ? "今天" : days === 1 ? "昨天" : `${days} 天前`; }
function OverviewState({ state }: { state: Exclude<DreamPageState, "ready"> }) {
  const copy = state === "loading" ? ["正在读取组织梦境…", "下一步：请稍候，页面会自动显示最新结果。", Clock3] : state === "error" ? ["暂时无法读取企业梦境", "下一步：检查网络后重试；历史结果没有被修改。", AlertTriangle] : ["还没有可查看的梦境结果", "下一步：到“梦境工作流”选择组织并设置整理方式。", MoonStar];
  const Icon = copy[2] as typeof Clock3;
  return <div className="dream-state" data-testid={`dream-overview-${state}`} aria-live="polite"><Icon size={22} /><strong>{copy[0] as string}</strong><span>{copy[1] as string}</span></div>;
}

export function DreamHeader({ title, description }: { title: string; description: string }) { return <header className="dream-page-header"><div><p className="console-route-kicker">企业梦境</p><h1>{title}</h1><p>{description}</p></div><span className="dream-status"><CheckCircle2 size={15} />运行机制正常</span></header>; }
export function DreamSubnav({ current }: { current: "overview" | "timeline" | "workflow" }) { return <nav className="dream-subnav" aria-label="企业梦境页面"><DreamNavLink to="/dream" current={current === "overview"}>梦境全景</DreamNavLink><DreamNavLink to="/dream/timeline" current={current === "timeline"}>梦境时间线</DreamNavLink><DreamNavLink to="/dream/workflow" current={current === "workflow"}>梦境工作流</DreamNavLink></nav>; }
function DreamNavLink({ to, current, children }: { to: string; current: boolean; children: React.ReactNode }) {
  const inRouter = useInRouterContext();
  const props = { "aria-current": current ? "page" as const : undefined };
  return inRouter ? <Link to={to} {...props}>{children}</Link> : <a href={to} {...props}>{children}</a>;
}
