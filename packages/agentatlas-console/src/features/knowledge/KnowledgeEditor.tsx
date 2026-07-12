import { AlertTriangle, BookOpen, ChevronRight, CircleCheck, FileText, Plus, ShieldCheck, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useNavigate, useParams } from "react-router-dom";

import {
  createChange,
  emptyContent,
  getChange,
  newResourceID,
  UnsafeContentError,
  updateChange,
  type ChangeDraft,
  type ChangeResourceType,
  type KnowledgeContent,
} from "../../api/changes";
import { ApiError } from "../../api/client";
import { useSession } from "../../app/session";
import { ChangeDiff } from "../governance/ChangeDiff";
import { findOrgScopeNode } from "./OrgScopeNav";
import { SOPStepsEditor } from "./SOPStepsEditor";

type SaveState = "unsaved" | "saving" | "saved" | "failed";
interface ConflictState { currentRevision: number; server: KnowledgeContent; mine: KnowledgeContent }

export function KnowledgeEditor({ kind = "knowledge_entry" }: { kind?: ChangeResourceType }) {
  const { orgUnitID, changeID } = useParams();
  const { session, advancedMode } = useSession();
  const navigate = useNavigate();
  const organization = findOrgScopeNode(session.org_tree, orgUnitID);
  const directEdit = session.permissions.includes(kind === "workflow" ? "workflow_edit" : "edit");
  const canSuggest = session.permissions.includes("suggest");
  const [content, setContent] = useState<KnowledgeContent>(() => emptyContent(kind));
  const [draft, setDraft] = useState<ChangeDraft | null>(null);
  const [saveState, setSaveState] = useState<SaveState>("unsaved");
  const [savedAt, setSavedAt] = useState<Date | null>(null);
  const [dirty, setDirty] = useState(false);
  const [loading, setLoading] = useState(Boolean(changeID));
  const [message, setMessage] = useState<string | null>(null);
  const [blockedMessage, setBlockedMessage] = useState<string | null>(null);
  const [conflict, setConflict] = useState<ConflictState | null>(null);
  const [saveCycle, setSaveCycle] = useState(0);
  const resourceID = useRef(newResourceID(kind));
  const contentRef = useRef(content);
  const revisionRef = useRef<number | null>(null);
  const changeIDRef = useRef<string | null>(changeID ?? null);
  const editGenerationRef = useRef(0);
  const savingRef = useRef(false);
  const skipLoadChangeIDRef = useRef<string | null>(null);

  useEffect(() => { contentRef.current = content; }, [content]);
  useEffect(() => { revisionRef.current = draft?.revision ?? null; }, [draft]);

  useEffect(() => {
    if (!changeID) return;
    if (skipLoadChangeIDRef.current === changeID) {
      skipLoadChangeIDRef.current = null;
      return;
    }
    let active = true;
    setLoading(true);
    getChange(changeID).then((record) => {
      if (!active) return;
      if (record.draft.org_unit_id !== orgUnitID || record.draft.permission_mode !== "direct_edit") {
        setBlockedMessage("你不能在这个范围编辑这份内容。");
        return;
      }
      if (record.draft.resource_type !== kind) {
        if (record.draft.resource_type === "sop" || record.draft.resource_type === "knowledge_entry") {
          const kindSegment = record.draft.resource_type === "sop" ? "/sop" : "";
          active = false;
          navigate(`/knowledge/${encodeURIComponent(orgUnitID)}/changes/${encodeURIComponent(changeID)}${kindSegment}/edit`, { replace: true });
        } else {
          setBlockedMessage("这份内容需要从对应的维护入口打开。");
        }
        return;
      }
      setDraft(record.draft);
      setContent(record.content);
      setSaveState("saved");
      setSavedAt(new Date(record.draft.updated_at));
      setDirty(false);
    }).catch((reason: unknown) => {
      if (!active) return;
      if (reason instanceof UnsafeContentError) setBlockedMessage(reason.message);
      else setMessage("暂时无法读取这份草稿，请返回后重试。");
    })
      .finally(() => active && setLoading(false));
    return () => { active = false; };
  }, [changeID, kind, navigate, orgUnitID]);

  const save = useCallback(async (revisionOverride?: number) => {
    if (!orgUnitID || !directEdit || conflict && revisionOverride === undefined) return;
    if (savingRef.current) return;
    savingRef.current = true;
    const savingGeneration = editGenerationRef.current;
    setSaveState("saving");
    try {
      let saved: ChangeDraft;
      const currentID = changeIDRef.current;
      if (currentID) {
        const revision = revisionOverride ?? revisionRef.current;
        if (!revision) throw new Error("缺少草稿版本");
        saved = await updateChange(currentID, revision, contentRef.current);
      } else {
        saved = await createChange({
          org_unit_id: orgUnitID,
          resource_type: kind,
          resource_id: resourceID.current,
          action: "update",
          base_version: 0,
          proposed_content: contentRef.current,
        });
        changeIDRef.current = saved.change_id;
        skipLoadChangeIDRef.current = saved.change_id;
        const kindSegment = kind === "sop" ? "/sop" : "";
        navigate(`/knowledge/${encodeURIComponent(orgUnitID)}/changes/${encodeURIComponent(saved.change_id)}${kindSegment}/edit`, { replace: true });
      }
      setDraft(saved);
      revisionRef.current = saved.revision;
      setConflict(null);
      if (editGenerationRef.current === savingGeneration) {
        setDirty(false);
        setSaveState("saved");
        setSavedAt(new Date(saved.updated_at));
      } else {
        setDirty(true);
        setSaveState("unsaved");
        setSaveCycle((cycle) => cycle + 1);
      }
    } catch (reason) {
      if (reason instanceof ApiError && reason.status === 409) {
        const details = isRecord(reason.details) ? reason.details : {};
        const diff = isRecord(details.diff) ? details.diff : {};
        const currentRevision = Number(details.current_revision);
        if (Number.isInteger(currentRevision) && currentRevision > 0) {
          setConflict({ currentRevision, server: asContent(diff.before), mine: contentRef.current });
        }
      }
      setSaveState("failed");
    } finally {
      savingRef.current = false;
    }
  }, [conflict, directEdit, kind, navigate, orgUnitID]);

  useEffect(() => {
    if (!directEdit || !dirty || conflict) return;
    const timer = window.setTimeout(() => { void save(); }, 800);
    return () => window.clearTimeout(timer);
  }, [conflict, content, directEdit, dirty, save, saveCycle]);

  useEffect(() => {
    const warn = (event: BeforeUnloadEvent) => {
      if (!dirty && saveState !== "saving" && saveState !== "failed") return;
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty, saveState]);

  const updateContent = (next: KnowledgeContent) => {
    editGenerationRef.current += 1;
    setContent(next);
    setDirty(true);
    setMessage(null);
    setSaveState("unsaved");
  };

  const submitSuggestion = async () => {
    if (!orgUnitID || !canSuggest) return;
    setSaveState("saving");
    try {
      await createChange({ org_unit_id: orgUnitID, resource_type: kind, resource_id: resourceID.current, action: "update", base_version: 0, proposed_content: content }, true);
      setDirty(false);
      setSaveState("saved");
      setSavedAt(new Date());
      setMessage("建议已提交给知识负责人");
    } catch {
      setSaveState("failed");
    }
  };

  const title = kind === "sop" ? "制作 SOP 流程" : "新建或修改知识";
  const statusLabel = useMemo(() => saveState === "saving" ? "正在保存"
    : saveState === "saved" && savedAt ? `已保存 ${savedAt.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false })}`
      : saveState === "failed" ? "保存失败，重试" : "尚未保存", [saveState, savedAt]);

  if (!orgUnitID || !session.org_unit_ids.includes(orgUnitID) || !organization?.selectable || (!directEdit && !canSuggest)) return <Navigate to="/knowledge" replace />;
  if (loading) return <section className="knowledge-state" aria-busy="true">正在读取草稿…</section>;
  if (blockedMessage) return <section className="knowledge-state" role="alert"><strong>{blockedMessage}</strong><span>这份内容没有被截断或修改，请联系高级维护人员处理。</span><Link className="knowledge-secondary-button" to={`/knowledge/${encodeURIComponent(orgUnitID)}`}>返回企业知识</Link></section>;

  return (
    <article className="knowledge-editor-page" aria-labelledby="knowledge-editor-title">
      <header className="knowledge-editor-header">
        <div><p className="knowledge-eyebrow">当前范围：{organization.name || "未命名组织"}</p><h1 id="knowledge-editor-title" className="title-display">{title}</h1><p>按页面顺序填写，系统会把内容保存为草稿，不会直接发布。</p></div>
        {directEdit ? (
          saveState === "failed" ? <button type="button" className="knowledge-save-state knowledge-save-retry" onClick={() => void save()}>{statusLabel}</button>
            : <span className="knowledge-save-state" aria-live="polite">{statusLabel}</span>
        ) : <span className="knowledge-save-state">填写完成后提交建议</span>}
      </header>
      <p className="knowledge-next-step">填写顺序：先补充内容并确认保存状态，然后检查影响范围。</p>
      {message ? <div className="knowledge-editor-message" role="status">{message}</div> : null}
      {conflict ? (
        <section className="knowledge-conflict" aria-labelledby="knowledge-conflict-title">
          <AlertTriangle aria-hidden size={22} />
          <div><h2 id="knowledge-conflict-title">发现其他人的新修改</h2><p>你的内容没有被覆盖。请比较两边内容，再决定是否重新应用。</p></div>
          <ChangeDiff before={conflict.server} after={conflict.mine} />
          <button className="knowledge-primary-button" type="button" onClick={() => void save(conflict.currentRevision)}>重新应用我的修改</button>
        </section>
      ) : (
        <div className="knowledge-editor-layout">
          <nav className="knowledge-editor-outline" aria-label="内容目录">
            <p className="knowledge-eyebrow">内容目录</p>
            <a href="#basic"><FileText aria-hidden size={17} />基本说明</a>
            <a href="#content"><BookOpen aria-hidden size={17} />{kind === "sop" ? "处理步骤" : "处理方法"}</a>
            <a href="#scope"><ShieldCheck aria-hidden size={17} />范围与依据</a>
          </nav>
          <main className="knowledge-editor-content">
            <section id="basic" className="knowledge-editor-card glass-rest">
              <p className="knowledge-eyebrow">基本说明</p>
              <label>知识名称<input aria-label="知识名称" value={content.title} onChange={(event) => updateContent({ ...content, title: event.currentTarget.value })} /></label>
              <label>简要说明<textarea aria-label="简要说明" value={content.summary} onChange={(event) => updateContent({ ...content, summary: event.currentTarget.value })} /></label>
            </section>
            <div id="content">
              {kind === "sop" ? <SOPStepsEditor steps={content.steps ?? [{ title: "", instruction: "" }]} onChange={(steps) => updateContent({ ...content, steps })} />
                : <KnowledgeSectionsEditor sections={content.sections?.length ? content.sections : [{ heading: "处理方法", body: "" }]} onChange={(sections) => updateContent({ ...content, sections })} />}
            </div>
          </main>
          <aside id="scope" className="knowledge-editor-context" aria-label="范围与依据">
            <section className="glass-rest"><h2>适用组织范围</h2><p>{organization.name || "未命名组织"}</p></section>
            <section className="glass-rest"><h2>影响范围</h2><p>发布前会再次检查受影响的员工、Agent 回答和 SOP。</p></section>
            <ReferenceEditor references={content.references ?? []} onChange={(references) => updateContent({ ...content, references })} />
            {session.advanced_mode_allowed && advancedMode ? <Link className="knowledge-advanced-link" to="/advanced/legacy/knowledge">高级维护 <ChevronRight aria-hidden size={16} /></Link> : null}
          </aside>
        </div>
      )}
      {!conflict ? (
        <footer className="knowledge-editor-footer">
          {directEdit ? (
            <button className="knowledge-primary-button" type="button" disabled={!draft || dirty || saveState !== "saved"} onClick={() => navigate(`/knowledge/${encodeURIComponent(orgUnitID)}/changes/${encodeURIComponent(draft!.change_id)}/review`)}>
              下一步：检查并发布 <ChevronRight aria-hidden size={18} />
            </button>
          ) : (
            <button className="knowledge-primary-button" type="button" disabled={!content.title.trim() || !content.summary.trim() || message === "建议已提交给知识负责人"} onClick={() => void submitSuggestion()}>
              <CircleCheck aria-hidden size={18} /> 提交纠错建议
            </button>
          )}
        </footer>
      ) : null}
    </article>
  );
}

function isRecord(value: unknown): value is Record<string, unknown> { return typeof value === "object" && value !== null && !Array.isArray(value); }
function asContent(value: unknown): KnowledgeContent { return isRecord(value) ? value as unknown as KnowledgeContent : emptyContent(); }

function KnowledgeSectionsEditor({ sections, onChange }: { sections: NonNullable<KnowledgeContent["sections"]>; onChange(sections: NonNullable<KnowledgeContent["sections"]>): void }) {
  const update = (index: number, patch: { heading?: string; body?: string }) => onChange(sections.map((section, candidate) => candidate === index ? { ...section, ...patch } : section));
  return (
    <section className="knowledge-section-editor" aria-labelledby="knowledge-sections-title">
      <div className="knowledge-editor-section-heading"><div><p className="knowledge-eyebrow">可读内容</p><h2 id="knowledge-sections-title">内容说明</h2></div><button className="knowledge-secondary-button" type="button" onClick={() => onChange([...sections, { heading: "", body: "" }])}><Plus aria-hidden size={17} />添加内容部分</button></div>
      <div className="knowledge-section-list">
        {sections.map((section, index) => <section className="knowledge-editor-card glass-rest" key={index}>
          <div className="knowledge-section-card-heading"><strong>第 {index + 1} 部分</strong><button type="button" aria-label={`删除第 ${index + 1} 部分`} disabled={sections.length === 1} onClick={() => onChange(sections.filter((_, candidate) => candidate !== index))}><Trash2 aria-hidden size={16} /></button></div>
          <label>小标题<input aria-label={`第 ${index + 1} 部分标题`} value={section.heading} onChange={(event) => update(index, { heading: event.currentTarget.value })} /></label>
          <label>具体内容<textarea aria-label={`第 ${index + 1} 部分内容`} value={section.body} onChange={(event) => update(index, { body: event.currentTarget.value })} /></label>
        </section>)}
      </div>
    </section>
  );
}

function ReferenceEditor({ references, onChange }: { references: string[]; onChange(references: string[]): void }) {
  return (
    <section className="glass-rest knowledge-reference-editor">
      <div className="knowledge-reference-heading"><h2>参考资料</h2><button type="button" aria-label="添加参考资料" onClick={() => onChange([...references, ""])}><Plus aria-hidden size={16} /></button></div>
      {references.length === 0 ? <p>还没有参考资料，可以按上方按钮添加。</p> : references.map((reference, index) => <div className="knowledge-reference-row" key={index}><label>第 {index + 1} 项<input aria-label={`第 ${index + 1} 项参考资料`} value={reference} onChange={(event) => onChange(references.map((item, candidate) => candidate === index ? event.currentTarget.value : item))} /></label><button type="button" aria-label={`删除第 ${index + 1} 项参考资料`} onClick={() => onChange(references.filter((_, candidate) => candidate !== index))}><Trash2 aria-hidden size={16} /></button></div>)}
    </section>
  );
}
