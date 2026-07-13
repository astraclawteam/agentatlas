import { useEffect, useState } from "react";
import { DreamHeader, DreamSubnav } from "./features/dream/DreamOverviewPage";
import { DreamPolicyWizard } from "./features/dream/DreamPolicyWizard";
import { adoptDreamPolicy, backfillDreamPolicy, checkDreamPolicy, decideDreamPolicy, disableDreamPolicy, publishDreamPolicy, submitDreamPolicyReview, type BasicDreamPolicy, type DreamPolicyLifecycle, type DreamWorkflowBinding } from "./api/dream";
import "./features/dream/dream.css";

const emptyPolicies: DreamPolicyLifecycle[] = [];

export function DreamPolicyPanel({ organizations = [], bindings = [], initialPolicies = emptyPolicies, advancedAllowed = false, advancedMode = false, onSubmit }: { organizations?: Array<{ id: string; name: string }>; bindings?: DreamWorkflowBinding[]; initialPolicies?: DreamPolicyLifecycle[]; advancedAllowed?: boolean; advancedMode?: boolean; currentUserID?: string; onSubmit?: (value: BasicDreamPolicy, policyHandle?: string) => DreamPolicyLifecycle | void | Promise<DreamPolicyLifecycle | void> }) {
  const [selectedOrg, setSelectedOrg] = useState(organizations[0]?.id ?? "");
  const [localPolicies, setLocalPolicies] = useState(initialPolicies);
  const orgPolicies = localPolicies.filter((item) => item.organization_id === selectedOrg || (!item.organization_id && organizations.length === 1));
  const [selectedHandle, setSelectedHandle] = useState("");
  const policy = selectedHandle ? orgPolicies.find((item) => item.handle === selectedHandle) ?? null : null;
  const orgBindings = bindings.filter((item) => item.organization_id === selectedOrg || (!item.organization_id && organizations.length === 1));
  const [editing, setEditing] = useState(!policy);
  const [error, setError] = useState("");
  const run = async (task: () => Promise<DreamPolicyLifecycle | void>) => { setError(""); try { const saved = await task(); if (saved) { const scoped = { ...saved, organization_id: saved.organization_id ?? selectedOrg }; setLocalPolicies((items) => [scoped, ...items.filter((item) => item.handle !== scoped.handle)]); setSelectedHandle(scoped.handle); } } catch (reason) { setError(reason instanceof Error ? reason.message : "操作没有完成"); } };
  useEffect(() => { setLocalPolicies(initialPolicies); }, [initialPolicies]);
  useEffect(() => { const first = localPolicies.find((item) => item.organization_id === selectedOrg || (!item.organization_id && organizations.length === 1)); setSelectedHandle(first?.handle ?? ""); setEditing(!first); }, [selectedOrg]);
  return <main className="dream-page"><DreamHeader title="梦境工作流" description="用简单选择配置企业运行记录的自动整理。" /><DreamSubnav current="workflow" />
    <section className="dream-toolbar glass-rest"><label>选择组织<select aria-label="选择组织" value={selectedOrg} onChange={(event) => setSelectedOrg(event.target.value)}>{organizations.map((org) => <option key={org.id} value={org.id}>{org.name}</option>)}</select></label><label>选择策略<select aria-label="选择策略" value={policy?.handle ?? ""} onChange={(event) => { setSelectedHandle(event.target.value); setEditing(!event.target.value); }}><option value="">新建策略</option>{orgPolicies.map((item) => <option key={item.handle} value={item.handle}>第 {item.version || item.revision + 1} 版 · {item.status}</option>)}</select></label></section>
    {editing && orgBindings.length ? <><DreamPolicyWizard organizations={organizations.filter((org) => org.id === selectedOrg)} bindings={orgBindings} initialPolicy={policy ?? undefined} advancedAllowed={advancedAllowed} advancedMode={advancedMode} onSubmit={(value) => run(async () => { const saved = await onSubmit?.(value, policy?.handle); setEditing(false); return saved; })} />{policy ? <button className="dream-secondary" onClick={() => setEditing(false)}>取消编辑</button> : null}</> : null}
    {editing && !orgBindings.length ? <div className="dream-state"><strong>还没有可用的已发布梦境工作流</strong>{advancedAllowed ? <a href="/dream/workflow/advanced">进入高级维护</a> : null}</div> : null}
    {!editing && policy?.cadence === "custom" ? <div className="dream-state"><strong>这是高级策略</strong><span>高级页面只维护隐藏的运行字段，复核与发布仍在这里完成。</span>{advancedAllowed ? <a href={`/dream/workflow/advanced?policy=${encodeURIComponent(policy.handle)}`}>进入高级维护</a> : null}</div> : null}
    {!editing && policy ? <>{policy.cadence !== "custom" ? <button className="dream-secondary" onClick={() => setEditing(true)}>编辑基础设置</button> : null}<DreamPolicyLifecycleControls policy={policy} advancedMode={advancedMode} onAdopt={() => run(() => adoptDreamPolicy(policy.handle, policy.revision))} onSubmitReview={() => run(async () => { await checkDreamPolicy(policy.handle, policy.revision); return submitDreamPolicyReview(policy.handle, policy.revision); })} onDecide={(decision) => run(() => decideDreamPolicy(policy.handle, policy.revision, decision))} onPublish={() => run(() => publishDreamPolicy(policy.handle, policy.revision))} onDisable={() => run(() => submitDreamPolicyReview(policy.handle, policy.revision, "disable"))} onFinalizeDisable={() => run(() => disableDreamPolicy(policy.handle, policy.revision))} onBackfill={(start, end) => run(async () => { await backfillDreamPolicy(policy.handle, start, end); })} /></> : null}
    {error ? <p className="dream-state" role="alert"><strong>操作没有完成</strong><span>{error}</span></p> : null}</main>;
}

export function DreamPolicyLifecycleControls({ policy, advancedMode = false, onAdopt, onSubmitReview, onDecide, onPublish, onDisable, onFinalizeDisable, onBackfill }: { policy: DreamPolicyLifecycle; currentUserID?: string; advancedMode?: boolean; onAdopt?: () => void; onSubmitReview: () => void; onDecide: (decision: "approve" | "reject") => void; onPublish: () => void; onDisable?: () => void; onFinalizeDisable?: () => void; onBackfill?: (start: string, end: string) => void }) {
  const [start, setStart] = useState(""); const [end, setEnd] = useState("");
  return <section className="dream-advanced" aria-label="梦境工作流发布状态"><h2>检查与发布</h2>
    {policy.permission_mode === "suggestion_only" ? <><p>这是员工提交的建议，需要维护人员接管后才能修改或提交复核。</p>{policy.can_adopt && onAdopt ? <button className="dream-primary" onClick={onAdopt}>接管并继续维护</button> : null}</> : null}
    {policy.status === "draft" && policy.permission_mode === "direct_edit" ? <button className="dream-primary" onClick={onSubmitReview}>检查并提交复核</button> : null}
    {policy.status === "review_pending" ? <><p>{policy.review_mode === "upward_review" ? "已提交给上级负责人复核" : "等待低风险单人确认"}</p>{policy.can_decide ? <div className="dream-actions"><button className="dream-secondary" onClick={() => onDecide("reject")}>驳回</button><button className="dream-primary" onClick={() => onDecide("approve")}>批准发布</button></div> : null}</> : null}
    {policy.status === "approved" && policy.pending_action === "publish" ? <button className="dream-primary" onClick={onPublish}>发布新版本</button> : null}
    {policy.status === "approved" && policy.pending_action === "disable" && onFinalizeDisable ? <button className="dream-primary" onClick={onFinalizeDisable}>完成停用</button> : null}
    {policy.status === "published" ? <><p>已发布第 {policy.version} 版。</p>{advancedMode ? <><label className="dream-field">补跑开始时间<input aria-label="补跑开始时间" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /></label><label className="dream-field">补跑结束时间<input aria-label="补跑结束时间" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></label><div className="dream-actions">{onDisable ? <button className="dream-secondary" onClick={onDisable}>申请停用</button> : null}{onBackfill ? <button className="dream-secondary" disabled={!start || !end} onClick={() => onBackfill(new Date(start).toISOString(), new Date(end).toISOString())}>补跑所选时间段</button> : null}</div></> : null}</> : null}
    {policy.status === "disabled" ? <p>这条工作流已停用。</p> : null}
  </section>;
}
