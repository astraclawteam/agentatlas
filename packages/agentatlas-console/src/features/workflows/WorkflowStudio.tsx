import { AlertTriangle, ArrowRight, GitBranch, Save, ShieldCheck } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";

import type { AtlasWorkflow } from "../../types";
import { AdvancedWorkflowStudio } from "./AdvancedWorkflowStudio";
import { isBasicLinearWorkflow, linearNodeOrder } from "./flowgram-adapter";
import { humanNodeLabel, SOPStructuredEditor } from "./SOPStructuredEditor";
import "./workflows.css";

export interface WorkflowDraftRecord {
  handle: string;
  name: string;
  status: "draft" | "published";
  revision: number;
  definition: AtlasWorkflow;
  can_edit: boolean;
}

export interface WorkflowStudioRepository {
  updateDraft(handle: string, revision: number, definition: AtlasWorkflow): Promise<WorkflowDraftRecord>;
  createDraftFromPublished(handle: string): Promise<WorkflowDraftRecord>;
  prepareReview(handle: string, orgUnitID: string, revision: number): Promise<{ change_id: string }>;
}

export function WorkflowStudio({ item, repository, orgUnitID, advancedMode, permissions }: {
  item: WorkflowDraftRecord; repository: WorkflowStudioRepository; orgUnitID: string; advancedMode: boolean; permissions: string[];
}) {
  const navigate = useNavigate();
  const [record, setRecord] = useState(item);
  const [draft, setDraft] = useState(item.definition);
  const [phase, setPhase] = useState<"ready" | "working" | "saved" | "conflict" | "error">("ready");
  const [latest, setLatest] = useState<WorkflowDraftRecord | null>(null);
  const canEdit = record.can_edit && permissions.includes("workflow_edit");
  const canUseAdvanced = advancedMode && permissions.includes("workflow_advanced");
  const linear = isBasicLinearWorkflow(draft);

  const beginPublishedEdit = async () => {
    setPhase("working");
    try {
      const created = await repository.createDraftFromPublished(record.handle);
      setRecord(created); setDraft(created.definition); setPhase("ready");
    } catch { setPhase("error"); }
  };
  const save = async () => {
    setPhase("working"); setLatest(null);
    try {
      const saved = await repository.updateDraft(record.handle, record.revision, draft);
      setRecord(saved); setDraft(saved.definition); setPhase("saved");
    } catch (reason) {
      const error = reason as { status?: number; details?: { latest?: WorkflowDraftRecord } };
      if (error.status === 409) { setLatest(error.details?.latest ?? null); setPhase("conflict"); }
      else setPhase("error");
    }
  };
  const review = async () => {
    setPhase("working");
    try {
      const result = await repository.prepareReview(record.handle, orgUnitID, record.revision);
      navigate(`/knowledge/${encodeURIComponent(orgUnitID)}/changes/${encodeURIComponent(result.change_id)}/review`);
    } catch { setPhase("error"); }
  };

  return (
    <article className="workflow-studio" aria-labelledby="workflow-title">
      <header className="workflow-page-header">
        <div><p className="workflow-eyebrow">当前范围：已授权组织</p><h1 id="workflow-title" className="title-display">{record.name}</h1><p>用于维护员工照着执行的标准做事流程。</p></div>
        <span className="workflow-state"><ShieldCheck aria-hidden size={17} />{record.status === "published" ? "已发布，只读" : phase === "saved" ? "草稿已保存" : "草稿"}</span>
      </header>
      <p className="workflow-next">下一步：{record.status === "published" ? "先创建新草稿，历史版本不会被修改。" : "核对步骤并保存，然后进入检查与发布。"}</p>

      {record.status === "published" ? (
        <section className="workflow-published glass-rest"><div><strong>这是已发布版本</strong><p>员工正在使用的内容不会直接被修改。开始修改会创建一份新草稿。</p></div>{canEdit ? <button className="workflow-primary" type="button" disabled={phase === "working"} onClick={() => void beginPublishedEdit()}>开始修改</button> : null}</section>
      ) : null}

      {!linear && !canUseAdvanced ? (
        <section className="workflow-readonly glass-rest"><GitBranch aria-hidden size={22} /><div><strong>这个流程包含分支或高级设置，基础模式只能查看。</strong><p>系统不会猜测步骤顺序，也不会把分支压成一条直线。</p><ol>{draft.nodes.map((node, index) => <li key={node.id}>{index + 1}. {humanNodeLabel(node)}</li>)}</ol></div>{permissions.includes("workflow_advanced") ? <button type="button" onClick={() => navigate(`/workflows/${encodeURIComponent(record.handle)}/advanced`)}>使用高级维护模式打开</button> : null}</section>
      ) : canUseAdvanced ? <AdvancedWorkflowStudio draft={draft} onChange={setDraft} readonly={!canEdit || record.status === "published"} />
        : <SOPStructuredEditor workflow={draft} disabled={!canEdit || record.status === "published"} onChange={setDraft} />}

      {phase === "conflict" ? <div className="workflow-message" role="alert"><AlertTriangle aria-hidden size={18} /><div><strong>其他人刚刚保存了新版本。你的修改尚未覆盖服务器内容。</strong><span>{latest ? `服务器当前为第 ${latest.revision} 次修改。` : "请重新读取最新内容后再应用你的修改。"}</span></div><button type="button">比较并重新应用</button></div> : null}
      {phase === "error" ? <div className="workflow-message" role="alert"><AlertTriangle aria-hidden size={18} />这一步没有完成，当前页面里的修改尚未保存，请重试。</div> : null}

      {record.status === "draft" && canEdit ? <footer className="workflow-actions"><button type="button" onClick={() => void save()} disabled={phase === "working"}><Save aria-hidden size={18} />保存草稿</button><button className="workflow-primary" type="button" onClick={() => void review()} disabled={phase === "working"}>下一步：检查并发布<ArrowRight aria-hidden size={18} /></button></footer> : null}
    </article>
  );
}
