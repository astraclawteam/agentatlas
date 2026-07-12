import { useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { accessDreamEvidence, annotateDreamRun, basicPolicyToDefinition, buildHierarchy, createDreamPolicy, getDreamRun, listDreamPolicies, listDreamRuns, rerunDreamRun, type BasicDreamPolicy, type DreamPolicyDefinition, type DreamRun } from "../../api/dream";
import { useSession, type OrgScopeNode } from "../../app/session";
import { DreamPolicyPanel } from "../../DreamPolicyPanel";
import { DreamTimeline } from "../../DreamTimeline";
import { DreamOverviewPage, type DreamPageState } from "./DreamOverviewPage";
import { DreamRunDetailPage } from "./DreamRunDetailPage";

export function DreamOverviewRoute() {
  const { session } = useSession();
  const { runs, state } = useAuthorizedRuns(session.org_tree);
  const latest = new Map<string, DreamRun | undefined>();
  for (const run of runs) if (!latest.has(run.org_unit_id)) latest.set(run.org_unit_id, run);
  return <DreamOverviewPage data={buildHierarchy(session.org_tree, latest)} state={state} />;
}

export function DreamTimelineRoute() {
  const { session } = useSession();
  const { runs, state } = useAuthorizedRuns(session.org_tree);
  const organizations = useMemo(() => flattenOrganizations(session.org_tree), [session.org_tree]);
  return <DreamTimeline runs={runs} organizations={organizations} state={state === "empty" ? "ready" : state} />;
}

export function DreamPolicyRoute() {
  const { session, advancedMode } = useSession();
  const [message, setMessage] = useState("");
  const submit = async (basic: BasicDreamPolicy) => {
    setMessage("");
    const current = await listDreamPolicies(basic.organization);
    const published = current.dream_policies[0]?.policy as DreamPolicyDefinition | undefined;
    if (!published?.workflow?.id || !published.output_space_id) {
      setMessage("这个组织还没有可复用的已发布梦境工作流，请让服务商先在高级维护模式中完成绑定。");
      return;
    }
    const created = await createDreamPolicy(basicPolicyToDefinition(basic, { workflowID: published.workflow.id, workflowVersion: published.workflow.version, outputSpaceID: published.output_space_id, timezone: published.timezone }));
    setMessage("梦境工作流草稿已保存。检查并确认后才会影响后续运行。");
    return created;
  };
  return <><DreamPolicyPanel organizations={flattenOrganizations(session.org_tree)} advancedAllowed={Boolean(session.advanced_mode_allowed)} advancedMode={advancedMode} currentUserID={session.enterprise_user_id} onSubmit={submit} />{message ? <p className="dream-state" role="status">{message}</p> : null}</>;
}

export function DreamRunDetailRoute() {
  const { runID = "" } = useParams();
  const { session } = useSession();
  const [run, setRun] = useState<DreamRun | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => { const controller = new AbortController(); setRun(null); setFailed(false); getDreamRun(runID, controller.signal).then(setRun).catch((error) => { if ((error as Error).name !== "AbortError") setFailed(true); }); return () => controller.abort(); }, [runID]);
  if (failed) return <DreamOverviewPage state="error" />;
  if (!run) return <DreamOverviewPage state="loading" />;
  return <DreamRunDetailPage run={run} organizationName={organizationName(session.org_tree, run.org_unit_id)} onAnnotate={(action, comment) => annotateDreamRun(run.run_id, action, comment)} onRerun={(key) => rerunDreamRun(run.run_id, key)} onEvidenceAccess={() => accessDreamEvidence(run.run_id)} />;
}

function useAuthorizedRuns(tree: OrgScopeNode[]) {
  const organizations = useMemo(() => flattenOrganizations(tree), [tree]);
  const [runs, setRuns] = useState<DreamRun[]>([]);
  const [state, setState] = useState<DreamPageState>("loading");
  useEffect(() => { const controller = new AbortController(); setRuns([]); setState("loading"); Promise.all(organizations.map((org) => listDreamRuns(org.id, undefined, controller.signal))).then((sets) => { const values = sets.flat().sort((a, b) => b.window_end.localeCompare(a.window_end)); setRuns(values); setState(values.length ? "ready" : "empty"); }).catch((error) => { if ((error as Error).name !== "AbortError") setState("error"); }); return () => controller.abort(); }, [organizations]);
  return { runs, state };
}

function flattenOrganizations(tree: OrgScopeNode[]): Array<{ id: string; name: string }> { return tree.flatMap((node) => [...(node.selectable ? [{ id: node.id, name: node.name }] : []), ...flattenOrganizations(node.children ?? [])]); }
function organizationName(tree: OrgScopeNode[], id: string): string { for (const node of tree) { if (node.id === id) return node.name; const child = organizationName(node.children ?? [], id); if (child !== "授权组织") return child; } return "授权组织"; }
