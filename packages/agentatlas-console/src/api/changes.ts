import { api } from "./client";

export type ChangeResourceType = "knowledge_entry" | "sop" | "workflow" | "dream_policy" | "method_outline";
export type ChangeState = "draft" | "submitted" | "approved" | "rejected" | "published" | "withdrawn";
export type ReviewMode = "single_confirmation" | "upward_review" | "enterprise_knowledge_admin_queue";

export interface KnowledgeSection { heading: string; body: string }
export interface SOPStep { title: string; instruction: string; evidence?: string; completion?: string }
export interface KnowledgeContent {
  title: string;
  summary: string;
  sections?: KnowledgeSection[];
  steps?: SOPStep[];
  references?: string[];
  impact?: { people?: number; agent_answers?: boolean; sops?: string[]; organizations?: string[] };
}

export interface ChangeDraft {
  change_id: string;
  org_unit_id: string;
  resource_type: ChangeResourceType;
  resource_id: string;
  requester_user_id: string;
  permission_mode: "direct_edit" | "suggestion_only";
  revision: number;
  state: ChangeState;
  proposed_content: KnowledgeContent;
  updated_at: string;
}

export interface ReviewRoute {
  reviewer_user_id?: string;
  risk_level: "low" | "high";
  mode: ReviewMode;
  state: "pending" | "approved" | "rejected" | "cancelled";
  org_path: string[];
  queue?: string;
}

export interface ChangeRecord {
  draft: ChangeDraft;
  content: KnowledgeContent;
  base_content: Partial<KnowledgeContent>;
  assessment?: RiskAssessment;
  route?: Partial<ReviewRoute>;
}

export interface ChangeDiffValue { before: Partial<KnowledgeContent>; after: KnowledgeContent }
export interface RiskAssessment { risk_level: "low" | "high"; risk_reasons: string[] }

interface ChangeInput {
  org_unit_id: string;
  resource_type: ChangeResourceType;
  resource_id: string;
  action: "create" | "update";
  base_version: number;
  proposed_content: KnowledgeContent;
}

export function createChange(input: ChangeInput, suggestionOnly = false) {
  return api<ChangeDraft>(suggestionOnly ? "/api/changes/suggestions" : "/api/changes", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export function updateChange(changeID: string, revision: number, proposedContent: KnowledgeContent) {
  return api<ChangeDraft>(changePath(changeID), {
    method: "PUT",
    body: JSON.stringify({ revision, proposed_content: proposedContent }),
  });
}

export async function getChange(changeID: string): Promise<ChangeRecord> {
  return normalizeRecord(await api<unknown>(changePath(changeID)));
}

export async function listChanges(orgUnitID: string): Promise<ChangeRecord[]> {
  const query = new URLSearchParams({ org_unit_id: orgUnitID, limit: "100" });
  const raw = await api<{ items?: unknown[] }>(`/api/changes?${query.toString()}`);
  return (raw.items ?? []).map(normalizeRecord);
}

export function diffChange(changeID: string) {
  return api<ChangeDiffValue>(`${changePath(changeID)}/diff`);
}

export function assessChange(changeID: string) {
  return api<RiskAssessment>(`${changePath(changeID)}/assess`, { method: "POST", body: "{}" });
}

export function submitChange(changeID: string) {
  return api<ReviewRoute>(`${changePath(changeID)}/submit`, { method: "POST", body: "{}" });
}

export function decideChange(changeID: string, decision: "approve" | "reject", idempotencyKey: string, comment = "") {
  return api<void>(`${changePath(changeID)}/decisions`, {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
    body: JSON.stringify({ decision, comment }),
  });
}

export async function publishChange(changeID: string, idempotencyKey: string) {
  const raw = await api<Record<string, unknown>>(`${changePath(changeID)}/publish`, {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
    body: "{}",
  });
  return {
    version: Number(raw.version ?? raw.Version ?? 0),
  };
}

function changePath(changeID: string) {
  return `/api/changes/${encodeURIComponent(changeID)}`;
}

function normalizeRecord(value: unknown): ChangeRecord {
  const raw = isRecord(value) ? value : {};
  const draft = (raw.draft ?? raw.Draft) as ChangeDraft | undefined;
  if (!draft) throw new Error("变更记录缺少草稿信息");
  return {
    draft,
    content: asContent(raw.content ?? raw.Content ?? draft.proposed_content),
    base_content: asContent(raw.base_content ?? raw.BaseContent ?? {}),
    assessment: (raw.assessment ?? raw.Assessment) as RiskAssessment | undefined,
    route: (raw.route ?? raw.Route) as Partial<ReviewRoute> | undefined,
  };
}

function asContent(value: unknown): KnowledgeContent {
  if (typeof value === "string") {
    try { return asContent(JSON.parse(value) as unknown); } catch { return emptyContent(); }
  }
  return isRecord(value) ? value as unknown as KnowledgeContent : emptyContent();
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function emptyContent(kind: ChangeResourceType = "knowledge_entry"): KnowledgeContent {
  return kind === "sop"
    ? { title: "", summary: "", steps: [{ title: "", instruction: "" }], references: [], impact: {} }
    : { title: "", summary: "", sections: [{ heading: "处理方法", body: "" }], references: [], impact: {} };
}

export function newResourceID(kind: ChangeResourceType) {
  const suffix = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `${kind}-${suffix}`;
}

export function operationKey(action: string, changeID: string, revision: number) {
  const canonical = `${action}\u0000${changeID}\u0000${revision}`;
  const digest = [0x811c9dc5, 0x9e3779b9, 0x85ebca6b, 0xc2b2ae35]
    .map((seed, index) => hash32(index % 2 ? [...canonical].reverse().join("") : canonical, seed).toString(16).padStart(8, "0"))
    .join("");
  return `atlas-${action}-${digest}`.slice(0, 128);
}

function hash32(value: string, seed: number) {
  let hash = seed >>> 0;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash;
}
