import { useEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";

import { loadKnowledgeHome, type KnowledgeHomeResponse } from "../../api/knowledge";
import { useSession } from "../../app/session";
import { KnowledgeList } from "./KnowledgeList";
import { KnowledgeSearch } from "./KnowledgeSearch";
import { firstSelectableNode, findOrgScopeNode, OrgScopeNav } from "./OrgScopeNav";
import { knowledgeTaskDefinitions } from "./KnowledgeTaskHandoff";
import "./knowledge.css";

export function KnowledgeHome() {
  const { orgUnitID } = useParams();
  const { session } = useSession();
  const allowed = useMemo(() => new Set(session.org_unit_ids), [session.org_unit_ids]);
  const fallback = firstSelectableNode(session.org_tree, allowed);
  const selectedID = orgUnitID && allowed.has(orgUnitID) ? orgUnitID : fallback?.id;
  const selectedNode = findOrgScopeNode(session.org_tree, selectedID);
  const organizationName = selectedNode?.name || "当前组织";
  const [searchExpanded, setSearchExpanded] = useState(false);
  const [query, setQuery] = useState("");
  const [request, setRequest] = useState<{ scope: string; state: "loading" | "success" | "error"; data?: KnowledgeHomeResponse } | null>(null);
  const [retry, setRetry] = useState(0);
  const requestGeneration = useRef(0);

  useEffect(() => {
    if (!selectedID) return;
    const controller = new AbortController();
    const generation = ++requestGeneration.current;
    setRequest({ scope: selectedID, state: "loading" });
    loadKnowledgeHome(selectedID, query, controller.signal)
      .then((value) => {
        if (generation === requestGeneration.current && !controller.signal.aborted) setRequest({ scope: selectedID, state: "success", data: value });
      })
      .catch(() => {
        if (generation === requestGeneration.current && !controller.signal.aborted) setRequest({ scope: selectedID, state: "error" });
      });
    return () => controller.abort();
  }, [query, retry, selectedID]);

  if (!selectedID) {
    return (
      <div className="knowledge-workspace">
        <OrgScopeNav nodes={session.org_tree} allowedOrgUnitIDs={session.org_unit_ids} countsAvailable={false} recentChanges={null} reviews={null} />
        <section className="knowledge-no-scope" role="alert">
          <h1>暂时没有可维护的知识范围</h1>
          <p>请联系企业负责人为你分配可维护的组织范围。</p>
        </section>
      </div>
    );
  }
  if (!orgUnitID || orgUnitID !== selectedID) return <Navigate to={`/knowledge/${encodeURIComponent(selectedID)}`} replace />;

  const currentRequest = request?.scope === selectedID ? request : null;
  const data = currentRequest?.state === "success" ? currentRequest.data : undefined;
  const counts = data?.counts ?? { available: false, recent_changes: null, reviews: null };
  const loading = !currentRequest || currentRequest.state === "loading";
  const error = currentRequest?.state === "error";
  return (
    <div className="knowledge-workspace">
      <OrgScopeNav nodes={session.org_tree} allowedOrgUnitIDs={session.org_unit_ids} countsAvailable={counts.available} recentChanges={counts.recent_changes} reviews={counts.reviews} />
      <section className="knowledge-home" aria-labelledby="knowledge-home-title">
        <header className="knowledge-home-header">
          <div>
            <p className="knowledge-eyebrow">当前维护范围</p>
            <h1 id="knowledge-home-title" className="title-display">{organizationName}的企业知识</h1>
            <p>这些知识会通过企业网关提供给获得授权的员工 Agent</p>
          </div>
          <div className="knowledge-running" data-running={data?.status.knowledge_runtime === "running" ? "true" : "false"}>
            <span aria-hidden className="knowledge-status-dot" />
            <span>{data?.status.knowledge_runtime === "running" ? "运行正常" : "正在检查"}</span>
            {data?.status.freshness_label ? <small>{data.status.freshness_label}</small> : null}
          </div>
        </header>

        <div className="glass-rest knowledge-purpose">
          <p>这里用于维护企业知识。可以补充资料、修正内容、制作 SOP，或处理员工建议。</p>
        </div>
        <p className="knowledge-next-step">下一步：选择上方的一项工作，或在已有内容中按标题搜索。</p>

        <section aria-labelledby="knowledge-actions-title">
          <h2 id="knowledge-actions-title" className="knowledge-section-title">你想做什么？</h2>
          <div className="knowledge-shortcuts">
            {knowledgeTaskDefinitions.map(({ label, help, icon: Icon, key, primary }) => (
              <Link key={key} to={`/knowledge/${encodeURIComponent(selectedID)}/${key}`} className="glass-action knowledge-shortcut" data-variant={primary ? "primary" : "secondary"}>
                <span className="knowledge-shortcut-icon"><Icon aria-hidden size={21} strokeWidth={1.8} /></span>
                <strong>{label}</strong>
                <small>{help}</small>
              </Link>
            ))}
          </div>
        </section>

        <section aria-labelledby="knowledge-list-title">
          <div className="knowledge-list-toolbar">
            <h2 id="knowledge-list-title" className="knowledge-section-title">已有内容</h2>
            <KnowledgeSearch expanded={searchExpanded} value={query} onExpandedChange={setSearchExpanded} onChange={setQuery} />
          </div>
          <KnowledgeList items={data?.items ?? []} loading={loading} error={error} organizationName={organizationName} query={query} onRetry={() => setRetry((value) => value + 1)} />
        </section>
      </section>
    </div>
  );
}
