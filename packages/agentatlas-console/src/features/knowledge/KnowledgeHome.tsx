import { useEffect, useMemo, useState } from "react";
import { ClipboardCheck, FilePenLine, Upload, Workflow } from "lucide-react";
import { Navigate, useParams } from "react-router-dom";

import { loadKnowledgeHome, type KnowledgeHomeResponse } from "../../api/knowledge";
import { useSession } from "../../app/session";
import { KnowledgeList } from "./KnowledgeList";
import { KnowledgeSearch } from "./KnowledgeSearch";
import { firstSelectableNode, OrgScopeNav } from "./OrgScopeNav";
import "./knowledge.css";

const shortcuts = [
  { label: "添加资料", help: "上传文件并整理为可用知识", icon: Upload, path: "upload", primary: true },
  { label: "新建或修改知识", help: "补充说明、修正错误内容", icon: FilePenLine, path: "edit", primary: false },
  { label: "制作 SOP 流程", help: "把做事方法整理成步骤", icon: Workflow, path: "sop", primary: false },
  { label: "处理建议与审核", help: "查看员工建议和待确认修改", icon: ClipboardCheck, path: "reviews", primary: false },
] as const;

export function KnowledgeHome() {
  const { orgUnitID } = useParams();
  const { session } = useSession();
  const allowed = useMemo(() => new Set(session.org_unit_ids), [session.org_unit_ids]);
  const fallback = firstSelectableNode(session.org_tree, allowed);
  const selectedID = orgUnitID && allowed.has(orgUnitID) ? orgUnitID : fallback?.id;
  const selectedNode = findNode(session.org_tree, selectedID);
  const organizationName = selectedNode?.name || "当前组织";
  const [searchExpanded, setSearchExpanded] = useState(false);
  const [query, setQuery] = useState("");
  const [data, setData] = useState<KnowledgeHomeResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    if (!selectedID) return;
    let current = true;
    setLoading(true);
    setError(false);
    loadKnowledgeHome(selectedID, query)
      .then((value) => { if (current) setData(value); })
      .catch(() => { if (current) setError(true); })
      .finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [query, retry, selectedID]);

  if (!selectedID) {
    return (
      <div className="knowledge-workspace">
        <OrgScopeNav nodes={session.org_tree} allowedOrgUnitIDs={session.org_unit_ids} recentChanges={0} reviews={0} />
        <section className="knowledge-no-scope" role="alert">
          <h1>暂时没有可维护的知识范围</h1>
          <p>请联系企业负责人为你分配可维护的组织范围。</p>
        </section>
      </div>
    );
  }
  if (!orgUnitID || orgUnitID !== selectedID) return <Navigate to={`/knowledge/${encodeURIComponent(selectedID)}`} replace />;

  const counts = data?.counts ?? { recent_changes: 0, reviews: 0 };
  const shownName = data?.organization.name || organizationName;
  return (
    <div className="knowledge-workspace">
      <OrgScopeNav nodes={session.org_tree} allowedOrgUnitIDs={session.org_unit_ids} recentChanges={counts.recent_changes} reviews={counts.reviews} />
      <section className="knowledge-home" aria-labelledby="knowledge-home-title">
        <header className="knowledge-home-header">
          <div>
            <p className="knowledge-eyebrow">当前维护范围</p>
            <h1 id="knowledge-home-title" className="title-display">{shownName}的企业知识</h1>
            <p>这些知识会通过企业网关提供给获得授权的员工 Agent</p>
          </div>
          <div className="knowledge-running" data-running={data?.status.running ? "true" : "false"}>
            <span aria-hidden className="knowledge-status-dot" />
            <span>{data?.status.running ? "运行正常" : "正在检查"}</span>
            {data?.status.freshness_label ? <small>{data.status.freshness_label}</small> : null}
          </div>
        </header>

        <div className="glass-rest knowledge-purpose">
          <p>这里用于维护企业知识。可以补充资料、修正内容、制作 SOP，或处理员工建议。</p>
        </div>

        <section aria-labelledby="knowledge-actions-title">
          <h2 id="knowledge-actions-title" className="knowledge-section-title">你想做什么？</h2>
          <div className="knowledge-shortcuts">
            {shortcuts.map(({ label, help, icon: Icon, path, primary }) => (
              <a key={path} href={`/knowledge/${encodeURIComponent(selectedID)}/${path}`} className="glass-action knowledge-shortcut" data-variant={primary ? "primary" : "secondary"}>
                <span className="knowledge-shortcut-icon"><Icon aria-hidden size={21} strokeWidth={1.8} /></span>
                <strong>{label}</strong>
                <small>{help}</small>
              </a>
            ))}
          </div>
        </section>

        <section aria-labelledby="knowledge-list-title">
          <div className="knowledge-list-toolbar">
            <h2 id="knowledge-list-title" className="knowledge-section-title">已有内容</h2>
            <KnowledgeSearch expanded={searchExpanded} value={query} onExpandedChange={setSearchExpanded} onChange={setQuery} />
          </div>
          <KnowledgeList items={data?.items ?? []} loading={loading} error={error} organizationName={shownName} query={query} onRetry={() => setRetry((value) => value + 1)} />
        </section>
      </section>
    </div>
  );
}

function findNode(nodes: import("../../app/session").OrgScopeNode[], id?: string): import("../../app/session").OrgScopeNode | undefined {
  if (!id) return undefined;
  for (const node of nodes) {
    if (node.id === id) return node;
    const child = findNode(node.children ?? [], id);
    if (child) return child;
  }
}
