import { api } from "./client";
import type { OrgScopeNode } from "../app/session";

export type DreamRunStatus = "pending" | "running" | "waiting_confirmation" | "succeeded" | "failed";
export type DreamSource = "work_brief" | "child_dream_summary" | "project_record" | "sop_update" | "agent_answer" | "external_evidence" | "completed_task" | "risk_event";

export interface DreamSignal { id: string; title: string; detail: string; severity: "info" | "warning" | "critical"; evidence_pointer_id?: string }
export interface DreamCoverage { expected_children: number; completed_children: number; input_count: number }
export interface DreamMissingInput { source_type: DreamSource; source_id: string; reason: "not_found" | "not_completed" | "not_authorized" | "failed" | "masked" }
export interface DreamRun {
  run_id: string;
  status: DreamRunStatus;
  org_unit_id: string;
  window_start: string;
  window_end: string;
  policy_version: number;
  workflow: { id: string; version: number };
  parent_run_ids: string[];
  input_count: number;
  coverage: DreamCoverage;
  missing_inputs: DreamMissingInput[];
  facts: DreamSignal[];
  themes: DreamSignal[];
  trends: DreamSignal[];
  risks: DreamSignal[];
  todos: DreamSignal[];
  display_summary: string;
  evidence_pointer_id: string;
  rerun_of_run_id?: string;
  input_snapshot: { source_counts: Array<{ source_type: DreamSource; count: number }>; sanitized_input_ids: string[] };
  visibility_snapshot: { visibility_level: "members" | "managers" | "company_sanitized"; org_unit_ids: string[]; masked_field_count: number };
  model_route: string;
  model_version: string;
  attempt: number;
  idempotency_key: string;
  failure_step?: string;
  annotations?: DreamAnnotation[];
}

export interface DreamAnnotation { action: "confirm" | "reject" | "mark_incorrect" | "comment"; comment?: string; created_at?: string; actor_name?: string }
export interface DreamHierarchyNode { org: Pick<OrgScopeNode, "id" | "name" | "selectable">; run?: DreamRun; children: DreamHierarchyNode[] }
export interface DreamPolicyDefinition {
  org_unit_id: string;
  timezone: string;
  schedule: string;
  input_sources: DreamSource[];
  workflow: { id: string; version: number };
  output_space_id: string;
  visibility_level: "members" | "managers" | "company_sanitized";
  masking_rules: string[];
  risk_signal_rules: string[];
  evidence_retention: "pointer_only" | "pointer_plus_display_summary";
  confirmation_mode: "always" | "high_risk_only" | "never";
  max_attempts: number;
  allow_partial_children: boolean;
}

export interface BasicDreamPolicy {
  organization: string;
  cadence: "nightly" | "weekly";
  inputSources: DreamSource[];
  visibility: DreamPolicyDefinition["visibility_level"];
  confirmation: DreamPolicyDefinition["confirmation_mode"];
}

export interface BasicPolicyContext { workflowID: string; workflowVersion: number; outputSpaceID: string; timezone?: string }
export interface DreamPolicyLifecycle {
  dream_policy_id: string; status: "draft" | "review_pending" | "approved" | "rejected" | "published" | "disabled";
  revision: number; version: number; requester_user_id: string; permission_mode: "direct_edit" | "suggestion_only";
  risk_level?: "low" | "high"; risk_reasons: string[]; review_mode?: "single_confirmation" | "upward_review" | "enterprise_knowledge_admin_queue";
  reviewer_user_id?: string; org_path: string[]; queue?: string; pending_action?: "publish" | "disable"; review_state?: "pending" | "approved" | "rejected";
  policy: DreamPolicyDefinition;
}

export function basicPolicyToDefinition(input: BasicDreamPolicy, context: BasicPolicyContext): DreamPolicyDefinition {
  const timezone = context.timezone ?? "Asia/Shanghai";
  return {
    org_unit_id: input.organization,
    timezone,
    schedule: input.cadence === "weekly" ? "0 22 * * 0" : "0 22 * * *",
    input_sources: input.inputSources,
    workflow: { id: context.workflowID, version: context.workflowVersion },
    output_space_id: context.outputSpaceID,
    visibility_level: input.visibility,
    masking_rules: [],
    risk_signal_rules: [],
    evidence_retention: "pointer_only",
    confirmation_mode: input.confirmation,
    max_attempts: 3,
    allow_partial_children: false,
  };
}

export function listDreamRuns(orgUnitID: string, window?: string, signal?: AbortSignal) {
  const query = new URLSearchParams({ org_unit_id: orgUnitID });
  if (window) query.set("window", window);
  return api<{ runs: DreamRun[] }>(`/api/dream/runs?${query}`, { signal }).then((value) => value.runs ?? []);
}

export function getDreamRun(runID: string, signal?: AbortSignal) {
  return api<DreamRun>(`/api/dream/runs/${encodeURIComponent(runID)}`, { signal });
}

export function annotateDreamRun(runID: string, action: DreamAnnotation["action"], comment = "", key = operationKey("annotation", runID, `${action}:${comment}`)) {
  return api<void>(`/api/dream/runs/${encodeURIComponent(runID)}/annotations`, {
    method: "POST", headers: { "Idempotency-Key": key }, body: JSON.stringify({ action, comment }),
  });
}

export function rerunDreamRun(runID: string, key = operationKey("rerun", runID, "pinned")) {
  return api<{ run_id: string }>(`/api/dream/runs/${encodeURIComponent(runID)}/reruns`, {
    method: "POST", headers: { "Idempotency-Key": key }, body: "{}",
  });
}

export function accessDreamEvidence(runID: string) {
  return api<{ sanitized_detail: string }>(`/api/dream/runs/${encodeURIComponent(runID)}/evidence-access`, { method: "POST", body: "{}" });
}

export function listDreamPolicies(orgUnitID: string) {
  return api<{ dream_policies: Array<Record<string, unknown>> }>(`/api/dream/policies?org_unit_id=${encodeURIComponent(orgUnitID)}`);
}

export function createDreamPolicy(policy: DreamPolicyDefinition, key = operationKey("policy", policy.org_unit_id, JSON.stringify(policy))) {
  return api<DreamPolicyLifecycle>("/api/dream/policies", {
    method: "POST", headers: { "Idempotency-Key": key }, body: JSON.stringify(policy),
  });
}
export function updateDreamPolicy(id: string, revision: number, policy: DreamPolicyDefinition) { return policyAction(id, "", "PUT", { revision, policy }, operationKey("policy-update", id, `${revision}:${JSON.stringify(policy)}`)); }
export function checkDreamPolicy(id: string, revision: number) { return api<{ revision: number; risk_level: "low" | "high"; risk_reasons: string[]; changed_fields: string[] }>(`${policyPath(id)}/check`, { method: "POST", headers: { "Idempotency-Key": operationKey("policy-check", id, String(revision)) }, body: JSON.stringify({ revision }) }); }
export function submitDreamPolicyReview(id: string, revision: number, action: "publish" | "disable" = "publish") { return policyAction(id, "review", "POST", { revision, action }, operationKey("policy-review", id, `${revision}:${action}`)); }
export function decideDreamPolicy(id: string, revision: number, decision: "approve" | "reject", comment = "") { return policyAction(id, "decisions", "POST", { revision, decision, comment }, operationKey("policy-decision", id, `${revision}:${decision}:${comment}`)); }
export function publishDreamPolicy(id: string, revision: number) { return policyAction(id, "publish", "POST", { revision }, operationKey("policy-publish", id, String(revision))); }
export function disableDreamPolicy(id: string, revision: number) { return policyAction(id, "disable", "POST", { revision }, operationKey("policy-disable", id, String(revision))); }
export function backfillDreamPolicy(id: string, windowStart: string, windowEnd: string, rerunOfRunID = "") { return api<{ run_id: string }>(`${policyPath(id)}/backfills`, { method: "POST", headers: { "Idempotency-Key": operationKey("policy-backfill", id, `${windowStart}:${windowEnd}:${rerunOfRunID}`) }, body: JSON.stringify({ window_start: windowStart, window_end: windowEnd, rerun_of_run_id: rerunOfRunID }) }); }
function policyAction(id: string, suffix: string, method: "POST" | "PUT", body: unknown, key: string) { return api<DreamPolicyLifecycle>(`${policyPath(id)}${suffix ? `/${suffix}` : ""}`, { method, headers: { "Idempotency-Key": key }, body: JSON.stringify(body) }); }
function policyPath(id: string) { return `/api/dream/policies/${encodeURIComponent(id)}`; }

export function operationKey(action: string, resource: string, payload: string) {
  let hash = 0x811c9dc5;
  for (const character of `${action}\u0000${resource}\u0000${payload}`) {
    hash ^= character.charCodeAt(0);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return `atlas-${action}-${hash.toString(16).padStart(8, "0")}`;
}

export function buildHierarchy(orgTree: OrgScopeNode[], runs: Map<string, DreamRun | undefined>): DreamHierarchyNode[] {
  return orgTree.map((org) => ({ org, run: runs.get(org.id), children: buildHierarchy(org.children ?? [], runs) }));
}
