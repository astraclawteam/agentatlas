import { api } from "./client";
import type { OrgScopeNode } from "../app/session";

export type DreamRunStatus = "pending" | "running" | "waiting_confirmation" | "succeeded" | "failed";
export type DreamSource = "work_brief" | "child_dream_summary" | "project_record" | "sop_update" | "agent_answer" | "external_evidence" | "completed_task" | "risk_event";
export interface DreamSignal { title: string; detail: string; severity: "info" | "warning" | "critical" }
export interface DreamCoverage { expected_children: number; completed_children: number; input_count: number }
export interface DreamAnnotation { action: "confirm" | "reject" | "mark_incorrect" | "comment"; comment?: string; created_at?: string; actor_name?: string }
export interface DreamLineage { organization_name: string; relation: string; handle?: string }

export interface DreamRun {
  handle: string;
  organization_id?: string;
  status: DreamRunStatus;
  window_start: string;
  window_end: string;
  input_count: number;
  coverage: DreamCoverage;
  missing_input_reasons: string[];
  facts: DreamSignal[];
  themes: DreamSignal[];
  trends: DreamSignal[];
  risks: DreamSignal[];
  todos: DreamSignal[];
  display_summary: string;
  rerun: boolean;
  failure_stage?: string;
  annotations?: DreamAnnotation[];
  input_organizations?: DreamLineage[];
  downstream_organizations?: DreamLineage[];
}

export interface DreamHierarchyNode { org: Pick<OrgScopeNode, "id" | "name" | "selectable">; run?: DreamRun; children: DreamHierarchyNode[] }
export interface BasicDreamPolicy {
  organization: string;
  cadence: "nightly" | "weekly";
  inputSources: DreamSource[];
  visibility: "members" | "managers" | "company_sanitized";
  confirmation: "always" | "high_risk_only" | "never";
  bindingHandle: string;
}
export interface DreamWorkflowBinding { handle: string; name: string; version_label: string; output_name: string; organization_id?: string }
export interface DreamPolicyLifecycle {
  handle: string;
  status: "draft" | "review_pending" | "approved" | "rejected" | "published" | "disabled";
  revision: number;
  version: number;
  permission_mode: "direct_edit" | "suggestion_only";
  risk_level?: "low" | "high";
  risk_reasons: string[];
  review_mode?: "single_confirmation" | "upward_review" | "enterprise_knowledge_admin_queue";
  pending_action?: "publish" | "disable";
  review_state?: "pending" | "approved" | "rejected";
  cadence: "nightly" | "weekly" | "custom";
  input_sources: DreamSource[];
  visibility: BasicDreamPolicy["visibility"];
  confirmation: BasicDreamPolicy["confirmation"];
  can_adopt: boolean;
  can_decide?: boolean;
}

export function listDreamRuns(orgUnitID: string, window?: string, signal?: AbortSignal) {
  const query = new URLSearchParams({ org_unit_id: orgUnitID });
  if (window) query.set("window", window);
  return api<{ runs: Omit<DreamRun, "organization_id">[] }>(`/api/dream/runs?${query}`, { signal }).then((value) => (value.runs ?? []).map((run) => ({ ...run, organization_id: orgUnitID })));
}
export function getDreamRun(handle: string, signal?: AbortSignal) { return api<DreamRun>(`/api/dream/runs/${encodeURIComponent(handle)}`, { signal }); }
export function annotateDreamRun(handle: string, action: DreamAnnotation["action"], comment = "", key = operationKey("annotation", handle, `${action}:${comment}`)) { return api<DreamAnnotation>(`/api/dream/runs/${encodeURIComponent(handle)}/annotations`, { method: "POST", headers: { "Idempotency-Key": key }, body: JSON.stringify({ action, comment }) }); }
export function rerunDreamRun(handle: string, key = operationKey("rerun", handle, "pinned")) { return api<{ handle: string }>(`/api/dream/runs/${encodeURIComponent(handle)}/reruns`, { method: "POST", headers: { "Idempotency-Key": key }, body: "{}" }); }
export function accessDreamEvidence(handle: string) { return api<{ sanitized_detail: string }>(`/api/dream/runs/${encodeURIComponent(handle)}/evidence-access`, { method: "POST", body: "{}" }); }

export function listDreamPolicies(orgUnitID: string, signal?: AbortSignal) { return api<{ dream_policies: DreamPolicyLifecycle[] }>(`/api/dream/policies?org_unit_id=${encodeURIComponent(orgUnitID)}`, { signal }).then((value) => value.dream_policies ?? []); }
export function listDreamWorkflowBindings(orgUnitID: string, signal?: AbortSignal) { return api<{ bindings: DreamWorkflowBinding[] }>(`/api/dream/workflow-bindings?org_unit_id=${encodeURIComponent(orgUnitID)}`, { signal }).then((value) => (value.bindings ?? []).map((binding) => ({ ...binding, organization_id: orgUnitID }))); }
export function createDreamPolicy(policy: BasicDreamPolicy, key = operationKey("policy", policy.organization, JSON.stringify(policy))) { return api<DreamPolicyLifecycle>("/api/dream/policies", { method: "POST", headers: { "Idempotency-Key": key }, body: JSON.stringify({ org_unit_id: policy.organization, cadence: policy.cadence, input_sources: policy.inputSources, visibility: policy.visibility, confirmation: policy.confirmation, binding_handle: policy.bindingHandle }) }); }
export function updateDreamPolicy(handle: string, revision: number, policy: BasicDreamPolicy) { return policyAction(handle, "", "PUT", { revision, org_unit_id: policy.organization, cadence: policy.cadence, input_sources: policy.inputSources, visibility: policy.visibility, confirmation: policy.confirmation, binding_handle: policy.bindingHandle }, operationKey("policy-update", handle, `${revision}:${JSON.stringify(policy)}`)); }
export function adoptDreamPolicy(handle: string, revision: number) { return policyAction(handle, "adoptions", "POST", { revision }, operationKey("policy-adopt", handle, String(revision))); }
export function checkDreamPolicy(handle: string, revision: number) { return api<{ revision: number; risk_level: "low" | "high"; risk_reasons: string[]; changed_fields: string[] }>(`${policyPath(handle)}/check`, { method: "POST", headers: { "Idempotency-Key": operationKey("policy-check", handle, String(revision)) }, body: JSON.stringify({ revision }) }); }
export function submitDreamPolicyReview(handle: string, revision: number, action: "publish" | "disable" = "publish") { return policyAction(handle, "review", "POST", { revision, action }, operationKey("policy-review", handle, `${revision}:${action}`)); }
export function decideDreamPolicy(handle: string, revision: number, decision: "approve" | "reject", comment = "") { return policyAction(handle, "decisions", "POST", { revision, decision, comment }, operationKey("policy-decision", handle, `${revision}:${decision}:${comment}`)); }
export function publishDreamPolicy(handle: string, revision: number) { return policyAction(handle, "publish", "POST", { revision }, operationKey("policy-publish", handle, String(revision))); }
export function disableDreamPolicy(handle: string, revision: number) { return policyAction(handle, "disable", "POST", { revision }, operationKey("policy-disable", handle, String(revision))); }
export function backfillDreamPolicy(handle: string, windowStart: string, windowEnd: string) { return api<{ handle: string }>(`${policyPath(handle)}/backfills`, { method: "POST", headers: { "Idempotency-Key": operationKey("policy-backfill", handle, `${windowStart}:${windowEnd}`) }, body: JSON.stringify({ window_start: windowStart, window_end: windowEnd }) }); }
function policyAction(handle: string, suffix: string, method: "POST" | "PUT", body: unknown, key: string) { return api<DreamPolicyLifecycle>(`${policyPath(handle)}${suffix ? `/${suffix}` : ""}`, { method, headers: { "Idempotency-Key": key }, body: JSON.stringify(body) }); }
function policyPath(handle: string) { return `/api/dream/policies/${encodeURIComponent(handle)}`; }

export function operationKey(action: string, resource: string, payload: string) { let hash = 0x811c9dc5; for (const character of `${action}\u0000${resource}\u0000${payload}`) { hash ^= character.charCodeAt(0); hash = Math.imul(hash, 0x01000193) >>> 0; } return `atlas-${action}-${hash.toString(16).padStart(8, "0")}`; }
export function buildHierarchy(orgTree: OrgScopeNode[], runs: Map<string, DreamRun | undefined>): DreamHierarchyNode[] { return orgTree.map((org) => ({ org, run: runs.get(org.id), children: buildHierarchy(org.children ?? [], runs) })); }
