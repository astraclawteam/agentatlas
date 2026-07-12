import { AlertCircle, ArrowRight, CheckCircle2, ShieldCheck, Users } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";

import {
  assessChange,
  decideChange,
  diffChange,
  getChange,
  listChanges,
  operationKey,
  publishChange,
  submitChange,
  UnsafeContentError,
  type ChangeDiffValue,
  type ChangeRecord,
  type ReviewRoute,
  type RiskAssessment,
} from "../../api/changes";
import { useSession, type OrgScopeNode } from "../../app/session";
import { findOrgScopeNode } from "../knowledge/OrgScopeNav";
import { ChangeDiff } from "./ChangeDiff";

type ActionPhase = "ready" | "working" | "submit_unknown" | "publish_retry" | "submitted" | "decided" | "published";

export function ChangeReviewPage() {
  const { orgUnitID, changeID } = useParams();
  const { session } = useSession();
  const organization = findOrgScopeNode(session.org_tree, orgUnitID);
  const [record, setRecord] = useState<ChangeRecord | null>(null);
  const [diff, setDiff] = useState<ChangeDiffValue | null>(null);
  const [assessment, setAssessment] = useState<RiskAssessment | null>(null);
  const [route, setRoute] = useState<Partial<ReviewRoute> | null>(null);
  const [phase, setPhase] = useState<ActionPhase>("ready");
  const [error, setError] = useState<string | null>(null);
  const decisionKey = record ? operationKey("approve", changeID ?? "", record.draft.revision) : "";
  const publishKey = record ? operationKey("publish", changeID ?? "", record.draft.revision) : "";

  useEffect(() => {
    if (!changeID) return;
    let active = true;
    Promise.all([getChange(changeID), diffChange(changeID), assessChange(changeID)])
      .then(([nextRecord, nextDiff, nextAssessment]) => {
        if (!active) return;
        if (nextRecord.draft.org_unit_id !== orgUnitID) throw new Error("wrong organization");
        setRecord(nextRecord);
        setDiff(nextDiff);
        setAssessment(nextAssessment);
        setRoute(nextRecord.route ?? null);
      })
      .catch((reason: unknown) => active && setError(reason instanceof UnsafeContentError ? reason.message : "暂时无法读取这次修改，请返回后重试。"));
    return () => { active = false; };
  }, [changeID, orgUnitID]);

  const publish = async () => {
    if (!changeID) return;
    setPhase("working");
    setError(null);
    try {
      await publishChange(changeID, publishKey);
      setPhase("published");
    } catch {
      setPhase("publish_retry");
      setError("发布结果暂时无法确认。你的草稿没有丢失，请使用同一次发布重试。");
    }
  };

  const runPrimary = async () => {
    if (!changeID || !record || !assessment) return;
    setPhase("working");
    setError(null);
    try {
      if (record.draft.state === "approved") {
        await publish();
        return;
      }
      if (record.draft.state === "submitted") {
        if (route?.mode === "single_confirmation") {
          await decideChange(changeID, "approve", decisionKey);
          setRecord((current) => current ? { ...current, draft: { ...current.draft, state: "approved" } } : current);
          await publish();
          return;
        }
        await decideChange(changeID, "approve", decisionKey);
        setPhase("decided");
        return;
      }
      let submittedRoute: ReviewRoute;
      try {
        submittedRoute = await submitChange(changeID);
      } catch {
        let recovered: ChangeRecord;
        try {
          recovered = await getChange(changeID);
        } catch {
          setPhase("submit_unknown");
          setError("无法确认提交结果，请刷新页面后查看最新状态。");
          return;
        }

        setRecord(recovered);
        setRoute(recovered.route ?? null);
        if (recovered.draft.state === "published") {
          setPhase("published");
          return;
        }
        if (recovered.draft.state === "approved") {
          if (recovered.draft.requester_user_id === session.enterprise_user_id
            && canEditResource(recovered.draft.resource_type, session.permissions)) {
            await publish();
          } else {
            setPhase("ready");
          }
          return;
        }
        if (recovered.draft.state === "submitted") {
          if (recovered.route?.mode === "single_confirmation"
            && recovered.draft.requester_user_id === session.enterprise_user_id
            && canEditResource(recovered.draft.resource_type, session.permissions)
            && session.permissions.includes("publish_low_risk")) {
            await decideChange(changeID, "approve", operationKey("approve", changeID, recovered.draft.revision));
            setRecord((current) => current ? { ...current, draft: { ...current.draft, state: "approved" } } : current);
            await publish();
          } else {
            setPhase("submitted");
          }
          return;
        }

        setPhase("ready");
        setError("提交尚未完成，可以重试。");
        return;
      }
      setRoute(submittedRoute);
      setRecord((current) => current ? { ...current, draft: { ...current.draft, state: "submitted" }, route: submittedRoute } : current);
      if (submittedRoute.mode !== "single_confirmation" || !session.permissions.includes("publish_low_risk")) {
        setPhase("submitted");
        return;
      }
      await decideChange(changeID, "approve", decisionKey);
      setRecord((current) => current ? { ...current, draft: { ...current.draft, state: "approved" } } : current);
      await publish();
    } catch {
      setPhase("ready");
      setError("这一步暂时没有完成，内容仍保留在草稿中，请稍后重试。" );
    }
  };

  const primary = useMemo(() => {
    if (!record || !assessment || phase === "submit_unknown" || phase === "submitted" || phase === "decided" || phase === "published") return null;
    if (phase === "publish_retry") return { label: "重试发布", action: publish };
    const canEdit = canEditResource(record.draft.resource_type, session.permissions);
    if (record.draft.state === "approved") {
      const canPublish = record.draft.requester_user_id === session.enterprise_user_id && canEdit;
      return canPublish ? { label: "发布已审核修改", action: runPrimary } : null;
    }
    if (record.draft.state === "submitted") {
      if (route?.mode === "single_confirmation") {
        const canConfirm = record.draft.requester_user_id === session.enterprise_user_id && canEdit && session.permissions.includes("publish_low_risk");
        return canConfirm ? { label: "继续确认并发布", action: runPrimary } : null;
      }
      const canReview = record.draft.requester_user_id !== session.enterprise_user_id
        && session.permissions.includes("approve_high_risk")
        && (route?.mode === "enterprise_knowledge_admin_queue" || route?.reviewer_user_id === session.enterprise_user_id);
      return canReview ? { label: "确认通过", action: runPrimary } : null;
    }
    if (record.draft.requester_user_id !== session.enterprise_user_id || record.draft.permission_mode !== "direct_edit") return null;
    if (!canEdit) return null;
    return assessment.risk_level === "low"
      ? session.permissions.includes("publish_low_risk")
        ? { label: "确认并发布", action: runPrimary }
        : { label: "提交审核", action: runPrimary }
      : { label: "提交审核", action: runPrimary };
  }, [assessment, phase, record, route, session.enterprise_user_id, session.permissions]);

  if (!orgUnitID || !changeID || !session.org_unit_ids.includes(orgUnitID) || !organization?.selectable) return <Navigate to="/knowledge" replace />;
  if (error && !record) return <section className="knowledge-state" role="alert"><strong>暂时无法打开检查页</strong><span>{error}</span><Link className="knowledge-secondary-button" to={`/knowledge/${encodeURIComponent(orgUnitID)}`}>返回企业知识</Link></section>;
  if (!record || !diff || !assessment) return <section className="knowledge-state" aria-busy="true">正在准备修改前后对比…</section>;

  const impact = record.content.impact ?? {};
  const impactOrganizations = humanImpactOrganizations(impact.organizations, session.org_tree, organization.name);
  const requiresSubmitReview = record.draft.state === "draft" && (assessment.risk_level === "high" || !session.permissions.includes("publish_low_risk"));
  return (
    <article className="change-review-page" aria-labelledby="change-review-title">
      <header className="change-review-header">
        <div><p className="knowledge-eyebrow">检查与发布</p><h1 id="change-review-title" className="title-display">检查修改并确认下一步</h1><p>先核对修改前后和影响范围。系统不会跳过必要的复核。</p></div>
        <span className="change-review-status"><ShieldCheck aria-hidden size={18} />{statusCopy(record.draft.state, phase)}</span>
      </header>
      <p className="knowledge-next-step">下一步：确认内容、风险和审批路径，再使用页面底部的唯一操作。</p>
      {error ? <div className="knowledge-editor-message" role="alert"><AlertCircle aria-hidden size={18} />{error}</div> : null}
      {phase === "submitted" ? <div className="knowledge-editor-message" role="status">{submittedStatusCopy(route)}</div> : null}
      {phase === "decided" ? <div className="knowledge-editor-message" role="status">复核结果已记录，发起人可以继续发布。</div> : null}
      {phase === "published" ? <div className="knowledge-editor-message" role="status"><CheckCircle2 aria-hidden size={18} />修改已发布</div> : null}
      {!primary && phase === "ready" ? <div className="knowledge-editor-message" role="status">{waitingActionCopy(record, assessment, session.enterprise_user_id, session.permissions)}</div> : null}

      <ChangeDiff before={diff.before} after={diff.after} />

      <div className="change-review-context">
        <section className="glass-rest"><h2>风险判断</h2><strong className={`risk-level risk-${assessment.risk_level}`}>{assessment.risk_level === "low" ? "低风险" : "高风险"}</strong><ul>{assessment.risk_reasons.map((reason) => <li key={reason}>{humanRiskReason(reason)}</li>)}</ul></section>
        <section className="glass-rest"><h2>生效后的影响</h2><ul className="impact-list">{impactOrganizations.map((name, index) => <li key={`${name}-${index}`}>{name}</li>)}{typeof impact.people === "number" ? <li><Users aria-hidden size={16} />{impact.people} 人</li> : <li>人数将在发布时复核</li>}{impact.agent_answers ? <li>员工 Agent 的相关回答会更新</li> : null}{(impact.sops ?? []).map((name) => <li key={name}>{name}</li>)}</ul></section>
        <section className="glass-rest"><h2>审批路径</h2><p>{reviewPathCopy(route, phase, organization.name, requiresSubmitReview)}</p></section>
      </div>

      {primary ? <footer className="change-review-footer"><button className="knowledge-primary-button" type="button" disabled={phase === "working"} onClick={() => void primary.action()}>{primary.label}<ArrowRight aria-hidden size={18} /></button></footer> : null}
    </article>
  );
}

export function ChangeReviewListPage() {
  const { orgUnitID } = useParams();
  const { session } = useSession();
  const organization = findOrgScopeNode(session.org_tree, orgUnitID);
  const [records, setRecords] = useState<ChangeRecord[] | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (!orgUnitID) return;
    let active = true;
    listChanges(orgUnitID).then((items) => active && setRecords(items)).catch(() => active && setFailed(true));
    return () => { active = false; };
  }, [orgUnitID]);
  if (!orgUnitID || !session.org_unit_ids.includes(orgUnitID) || !organization?.selectable) return <Navigate to="/knowledge" replace />;
  const actionable = (records ?? []).filter((record) => isActionable(record, session.enterprise_user_id, session.permissions));
  return (
    <section className="change-review-list" aria-labelledby="change-review-list-title">
      <p className="knowledge-eyebrow">当前范围：{organization.name || "未命名组织"}</p>
      <h1 id="change-review-list-title" className="title-display">处理建议与审核</h1>
      <p>这里只显示当前组织中需要你处理的建议或修改。</p>
      <p className="knowledge-next-step">下一步：选择一项修改，检查前后内容和影响范围。</p>
      {failed ? <div className="knowledge-state" role="alert"><strong>暂时无法读取待办</strong><span>没有作出任何审核决定，请稍后重试。</span></div>
        : records === null ? <div className="knowledge-state" aria-busy="true">正在读取需要处理的修改…</div>
          : actionable.length === 0 ? <div className="knowledge-state"><strong>现在没有需要你处理的修改</strong><span>你可以返回企业知识继续维护内容。</span></div>
            : <ul className="change-review-list-items">{actionable.map((record) => <li className="glass-rest" key={record.draft.change_id}><div><strong>{record.content.title || "未命名内容"}</strong><small>{record.draft.permission_mode === "suggestion_only" ? "员工纠错建议" : "待复核修改"}</small></div><Link className="knowledge-secondary-button" to={`/knowledge/${encodeURIComponent(orgUnitID)}/changes/${encodeURIComponent(record.draft.change_id)}/review`}>检查修改</Link></li>)}</ul>}
    </section>
  );
}

function isActionable(record: ChangeRecord, userID: string, permissions: string[]) {
  if (record.draft.permission_mode === "suggestion_only") return permissions.includes("edit");
  if (record.draft.state !== "submitted" || record.draft.requester_user_id === userID || !permissions.includes("approve_high_risk")) return false;
  return record.route?.mode === "enterprise_knowledge_admin_queue" || record.route?.reviewer_user_id === userID;
}

function humanRiskReason(reason: string) {
  const labels: Record<string, string> = {
    "bounded content change": "内容范围有限",
    "changed approvals": "修改了审批规则",
    "changed permissions": "修改了权限规则",
    "changed visibility": "修改了可见范围",
    "changed masking_rules": "修改了脱敏规则",
    "changed external_action": "修改了对外操作",
    "published workflow behavior": "会改变已发布流程的执行方式",
  };
  return labels[reason] ?? "涉及需要复核的业务内容";
}

function reviewPathCopy(route: Partial<ReviewRoute> | null, phase: ActionPhase, organizationName: string, requiresSubmitReview: boolean) {
  if (route?.mode === "enterprise_knowledge_admin_queue") return "审批路径：当前组织 → 企业知识管理员";
  if (route?.mode === "upward_review") return `审批路径：${organizationName || "当前组织"} → 上级组织`;
  if (requiresSubmitReview && !route) return "审批路径：提交后由系统确定复核负责人";
  if (phase === "submitted") return "审批路径：等待系统确定复核负责人";
  return "审批路径：当前维护人员确认后发布";
}

function submittedStatusCopy(route: Partial<ReviewRoute> | null) {
  if (route?.mode === "enterprise_knowledge_admin_queue") return "已提交给企业知识管理员复核";
  if (route?.mode === "upward_review") return "已提交给上级负责人复核";
  return "已提交，等待负责人复核";
}

function statusCopy(state: string, phase: ActionPhase) {
  if (phase === "published" || state === "published") return "已发布";
  if (phase === "submitted" || state === "submitted") return "待复核";
  if (phase === "decided" || state === "approved") return "已审核";
  return "草稿";
}

function canEditResource(resourceType: string, permissions: string[]) {
  return permissions.includes(resourceType === "workflow" ? "workflow_edit" : "edit");
}

function humanImpactOrganizations(organizationIDs: string[] | undefined, orgTree: OrgScopeNode[], fallbackName: string) {
  const ids = [...new Set(organizationIDs ?? [])];
  if (ids.length === 0) return [fallbackName || "未命名组织"];
  const names: string[] = [];
  let unknown = 0;
  for (const id of ids) {
    const node = findOrgScopeNode(orgTree, id);
    if (!node) {
      unknown += 1;
    } else {
      names.push(node.name || "未命名组织");
    }
  }
  if (unknown) names.push(`其他相关组织（${unknown} 个）`);
  return names;
}

function waitingActionCopy(record: ChangeRecord, assessment: RiskAssessment, userID: string, permissions: string[]) {
  const requester = record.draft.requester_user_id === userID;
  if (record.draft.state === "approved") {
    if (!requester) return "等待原发起人完成发布";
    if (!canEditResource(record.draft.resource_type, permissions)) return "当前账号没有发布这项内容的权限";
  }
  if (record.draft.state === "draft" && requester && !canEditResource(record.draft.resource_type, permissions)) return "当前账号没有提交这项内容的权限";
  if (record.draft.state === "draft" && requester && assessment.risk_level === "low" && !permissions.includes("publish_low_risk")) return "等待有发布权限的负责人处理";
  if (record.draft.state === "submitted" && requester) return "修改正在等待复核";
  return "当前没有需要你执行的操作";
}
