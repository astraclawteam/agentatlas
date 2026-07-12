import { ClipboardCheck, FilePenLine, Upload, Workflow } from "lucide-react";
import { Link, Navigate, useParams } from "react-router-dom";

import { useSession } from "../../app/session";
import { findOrgScopeNode } from "./OrgScopeNav";

export const knowledgeTaskDefinitions = [
  { key: "upload", label: "添加资料", help: "上传文件并整理为可用知识", icon: Upload, purpose: "把已有文件补充到当前组织的企业知识中。", next: "资料导入功能正在安全接入；现在不会上传或保存文件。请返回企业知识，或使用 Atlas 助手说明要补充的资料。", primary: true },
  { key: "edit", label: "新建或修改知识", help: "补充说明、修正错误内容", icon: FilePenLine, purpose: "为当前组织补充知识说明，或修正已有内容。", next: "结构化编辑将在下一阶段接入；当前页面不会创建草稿。请返回企业知识选择已有内容。", primary: false },
  { key: "sop", label: "制作 SOP 流程", help: "把做事方法整理成步骤", icon: Workflow, purpose: "把当前组织的做事方法整理成容易执行的步骤。", next: "SOP 编辑将在工作流页面接入；当前页面不会发布流程。请返回企业知识。", primary: false },
  { key: "reviews", label: "处理建议与审核", help: "查看员工建议和待确认修改", icon: ClipboardCheck, purpose: "处理当前组织收到的建议和分配给你的审核。", next: "审核工作台将在治理页面接入；当前页面不会作出审批决定。请返回企业知识。", primary: false },
] as const;

export function KnowledgeTaskHandoff() {
  const { orgUnitID, task } = useParams();
  const { session } = useSession();
  const definition = knowledgeTaskDefinitions.find((candidate) => candidate.key === task);
  const node = findOrgScopeNode(session.org_tree, orgUnitID);
  if (!orgUnitID || !session.org_unit_ids.includes(orgUnitID) || !node?.selectable || !definition) {
    return <Navigate to="/knowledge" replace />;
  }
  const Icon = definition.icon;
  return (
    <section className="knowledge-handoff" aria-labelledby="knowledge-handoff-title">
      <span className="knowledge-handoff-icon"><Icon aria-hidden size={24} strokeWidth={1.8} /></span>
      <p className="knowledge-eyebrow">当前范围：{node.name || "未命名组织"}</p>
      <h1 id="knowledge-handoff-title" className="title-display">{definition.label}</h1>
      <p>{definition.purpose}</p>
      <div className="glass-rest knowledge-handoff-next">下一步：{definition.next}</div>
      <Link className="knowledge-secondary-button" to={`/knowledge/${encodeURIComponent(orgUnitID)}`}>返回企业知识</Link>
    </section>
  );
}
