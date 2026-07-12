import { Building2, ChevronRight, Clock3, ClipboardCheck } from "lucide-react";
import { NavLink } from "react-router-dom";

import type { OrgScopeNode } from "../../app/session";

interface OrgScopeNavProps {
  nodes: OrgScopeNode[];
  allowedOrgUnitIDs: string[];
  countsAvailable: boolean;
  recentChanges: number | null;
  reviews: number | null;
}

export function OrgScopeNav({ nodes, allowedOrgUnitIDs, countsAvailable, recentChanges, reviews }: OrgScopeNavProps) {
  const allowed = new Set(allowedOrgUnitIDs);
  const visibleNodes = filterAuthorizedNodes(nodes, allowed);
  return (
    <nav className="knowledge-scope-nav" aria-label="知识范围">
      <p className="knowledge-eyebrow">选择知识适用范围</p>
      <OrgNodes nodes={visibleNodes} />
      <div className="knowledge-attention" aria-label="需要关注">
        <p className="knowledge-eyebrow">需要关注</p>
        <div className="knowledge-attention-link">
          <Clock3 aria-hidden size={17} strokeWidth={1.8} />
          <span>最近修改 {countsAvailable ? recentChanges ?? 0 : "暂时无法获取"}</span>
          <ChevronRight aria-hidden size={15} />
        </div>
        <div className="knowledge-attention-link">
          <ClipboardCheck aria-hidden size={17} strokeWidth={1.8} />
          <span>需要我审核 {countsAvailable ? reviews ?? 0 : "暂时无法获取"}</span>
          <ChevronRight aria-hidden size={15} />
        </div>
      </div>
    </nav>
  );
}

function OrgNodes({ nodes }: { nodes: OrgScopeNode[] }) {
  return (
    <ul className="knowledge-org-list">
      {nodes.map((node) => (
        <li key={node.id}>
          {node.selectable ? (
            <NavLink to={`/knowledge/${encodeURIComponent(node.id)}`} className="knowledge-org-link">
              <Building2 aria-hidden size={17} strokeWidth={1.8} />
              <span>{node.name || "未命名组织"}</span>
            </NavLink>
          ) : (
            <span className="knowledge-org-link knowledge-org-context">
              <Building2 aria-hidden size={17} strokeWidth={1.8} />
              <span>{node.name || "未命名组织"}</span>
            </span>
          )}
          {node.children?.length ? <OrgNodes nodes={node.children} /> : null}
        </li>
      ))}
    </ul>
  );
}

export function filterAuthorizedNodes(nodes: OrgScopeNode[], allowed: Set<string>): OrgScopeNode[] {
  return nodes.flatMap((node) => {
    const children = filterAuthorizedNodes(node.children ?? [], allowed);
    if (!allowed.has(node.id) && children.length === 0) return [];
    return [{ ...node, selectable: Boolean(node.selectable && allowed.has(node.id)), children }];
  });
}

export function firstSelectableNode(nodes: OrgScopeNode[], allowed: Set<string>): OrgScopeNode | undefined {
  for (const node of nodes) {
    if (node.selectable && allowed.has(node.id)) return node;
    const child = firstSelectableNode(node.children ?? [], allowed);
    if (child) return child;
  }
}

export function findOrgScopeNode(nodes: OrgScopeNode[], id?: string): OrgScopeNode | undefined {
  if (!id) return undefined;
  for (const node of nodes) {
    if (node.id === id) return node;
    const child = findOrgScopeNode(node.children ?? [], id);
    if (child) return child;
  }
}
