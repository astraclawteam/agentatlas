import { useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { accessDreamEvidence, annotateDreamRun, buildHierarchy, createDreamPolicy, getDreamRun, listDreamPolicies, listDreamRuns, listDreamWorkflowBindings, rerunDreamRun, updateDreamPolicy, type BasicDreamPolicy, type DreamPolicyLifecycle, type DreamRun, type DreamWorkflowBinding } from "../../api/dream";
import { useSession, type OrgScopeNode } from "../../app/session";
import { DreamPolicyPanel } from "../../DreamPolicyPanel";
import { DreamTimeline } from "../../DreamTimeline";
import { DreamOverviewPage, type DreamPageState } from "./DreamOverviewPage";
import { DreamRunDetailPage } from "./DreamRunDetailPage";

export function DreamOverviewRoute() { const { session } = useSession(); const { runs, state } = useAuthorizedRuns(session.org_tree); const latest = new Map<string, DreamRun | undefined>(); for (const run of runs) if (run.organization_id && !latest.has(run.organization_id)) latest.set(run.organization_id, run); return <DreamOverviewPage data={buildHierarchy(session.org_tree, latest)} state={state} />; }
export function DreamTimelineRoute() { const { session } = useSession(); const { runs, state } = useAuthorizedRuns(session.org_tree); const organizations = useMemo(() => flattenOrganizations(session.org_tree), [session.org_tree]); return <DreamTimeline runs={runs} organizations={organizations} state={state === "empty" ? "ready" : state} />; }

export function DreamPolicyRoute() {
  const { session, advancedMode } = useSession();
  const organizations = useMemo(() => flattenOrganizations(session.org_tree), [session.org_tree]);
  const [policies, setPolicies] = useState<DreamPolicyLifecycle[]>([]); const [bindings, setBindings] = useState<DreamWorkflowBinding[]>([]); const [message, setMessage] = useState("");
  useEffect(() => { const controller = new AbortController(); Promise.all(organizations.map(async (org) => ({ policies: await listDreamPolicies(org.id, controller.signal), bindings: await listDreamWorkflowBindings(org.id, controller.signal) }))).then((sets) => { setPolicies(sets.flatMap((item) => item.policies)); setBindings(sets.flatMap((item) => item.bindings)); }).catch((error) => { if ((error as Error).name !== "AbortError") setMessage("暂时无法读取梦境工作流，请稍后重试。"); }); return () => controller.abort(); }, [organizations]);
  const submit = async (basic: BasicDreamPolicy) => { const current = policies.find((item) => item.status !== "disabled"); const saved = current ? await updateDreamPolicy(current.handle, current.revision, basic) : await createDreamPolicy(basic); setPolicies((items) => [saved, ...items.filter((item) => item.handle !== saved.handle)]); setMessage(current ? "修改已保存，重新打开页面仍会保留当前状态。" : "梦境工作流草稿已保存，复核后才会影响运行。"); return saved; };
  return <><DreamPolicyPanel organizations={organizations} bindings={bindings} initialPolicies={policies} advancedAllowed={Boolean(session.advanced_mode_allowed)} advancedMode={advancedMode} currentUserID={session.enterprise_user_id} onSubmit={submit} />{message ? <p className="dream-state" role="status">{message}</p> : null}</>;
}

export function DreamRunDetailRoute() { const { runID = "" } = useParams(); const [run, setRun] = useState<DreamRun | null>(null); const [failed, setFailed] = useState(false); useEffect(() => { const controller = new AbortController(); setRun(null); setFailed(false); getDreamRun(runID, controller.signal).then(setRun).catch((error) => { if ((error as Error).name !== "AbortError") setFailed(true); }); return () => controller.abort(); }, [runID]); if (failed) return <DreamOverviewPage state="error" />; if (!run) return <DreamOverviewPage state="loading" />; return <DreamRunDetailPage run={run} onAnnotate={async (action, comment) => { await annotateDreamRun(run.handle, action, comment); setRun(await getDreamRun(run.handle)); }} onRerun={(key) => rerunDreamRun(run.handle, key)} onEvidenceAccess={() => accessDreamEvidence(run.handle)} />; }

function useAuthorizedRuns(tree: OrgScopeNode[]) { const organizations = useMemo(() => flattenOrganizations(tree), [tree]); const [runs, setRuns] = useState<DreamRun[]>([]); const [state, setState] = useState<DreamPageState>("loading"); useEffect(() => { const controller = new AbortController(); setRuns([]); setState("loading"); Promise.all(organizations.map((org) => listDreamRuns(org.id, undefined, controller.signal))).then((sets) => { const values = sets.flat().sort((a, b) => b.window_end.localeCompare(a.window_end)); setRuns(values); setState(values.length ? "ready" : "empty"); }).catch((error) => { if ((error as Error).name !== "AbortError") setState("error"); }); return () => controller.abort(); }, [organizations]); return { runs, state }; }
function flattenOrganizations(tree: OrgScopeNode[]): Array<{ id: string; name: string }> { return tree.flatMap((node) => [...(node.selectable ? [{ id: node.id, name: node.name }] : []), ...flattenOrganizations(node.children ?? [])]); }
