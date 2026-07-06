// 统一后端客户端：票据是唯一鉴权路径（X-Nexus-Ticket，存 localStorage），
// atlas-api（运行时面 :8080）与 atlas-agent（控制面 :8081）双基址。
import type { AnswerTraceView, AtlasWorkflow, KnowledgeSpace, TimelineNode } from "./types";

const env = (import.meta as any).env ?? {};
export const API_BASE: string = env.VITE_ATLAS_API_URL ?? "http://localhost:8080";
export const AGENT_BASE: string = env.VITE_ATLAS_AGENT_URL ?? "http://localhost:8081";

const TICKET_KEY = "atlas_console_ticket";

export function getTicket(): string {
  return localStorage.getItem(TICKET_KEY) ?? "";
}

export function setTicket(v: string) {
  localStorage.setItem(TICKET_KEY, v);
}

async function request<T>(base: string, path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(base + path, {
    ...init,
    headers: {
      ...(init?.body instanceof FormData ? {} : { "Content-Type": "application/json" }),
      "X-Nexus-Ticket": getTicket(),
      ...(init?.headers ?? {}),
    },
  });
  const raw = await resp.text();
  let body: any = {};
  try {
    body = raw ? JSON.parse(raw) : {};
  } catch {
    body = { message: raw.slice(0, 200) };
  }
  if (!resp.ok) {
    throw new Error(body.message ?? `HTTP ${resp.status}`);
  }
  return body as T;
}

// --- 运行时面（atlas-api） ---------------------------------------------------

export function listSpaces() {
  return request<{ spaces: KnowledgeSpace[] }>(API_BASE, "/v1/spaces");
}

export function listTimeline(spaceID: string) {
  return request<{ nodes: TimelineNode[] }>(API_BASE, `/v1/spaces/${spaceID}/timeline`);
}

export function listRecentTraces() {
  return request<{ traces: AnswerTraceView[] }>(API_BASE, "/v1/traces");
}

/** 上传一份资料并创建解析任务（atlas-worker 异步解析入库）。 */
export async function uploadArtifact(file: File): Promise<{ artifact_id: string; job_id: string }> {
  const form = new FormData();
  form.append("file", file, file.name);
  form.append("content_type", file.type || "application/octet-stream");
  form.append("parser_hint", "auto");
  return request(API_BASE, "/v1/artifacts/jobs", { method: "POST", body: form });
}

// --- 控制面（atlas-agent） ---------------------------------------------------

export interface AgentRunResult {
  run_id: string;
  text: string;
  tool_calls?: Array<{ name: string; args?: unknown; result?: unknown }>;
}

export function agentRun(message: string) {
  return request<AgentRunResult>(AGENT_BASE, "/v1/agent/runs", {
    method: "POST",
    body: JSON.stringify({ message }),
  });
}

export function agentMessage(runID: string, message: string) {
  return request<AgentRunResult>(AGENT_BASE, `/v1/agent/runs/${runID}/messages`, {
    method: "POST",
    body: JSON.stringify({ message }),
  });
}

export function createWorkflow(name: string, definition: AtlasWorkflow) {
  return request<{ workflow_id: string }>(AGENT_BASE, "/v1/workflows", {
    method: "POST",
    body: JSON.stringify({ name, definition }),
  });
}

export function publishWorkflow(workflowID: string) {
  return request<{ workflow_id: string; version: number }>(AGENT_BASE, `/v1/workflows/${workflowID}/publish`, {
    method: "POST",
    body: JSON.stringify({}),
  });
}

export function listDreamPolicies() {
  return request<{ dream_policies: Array<{ dream_policy_id: string; org_scope: string; status: string; policy?: Record<string, any> }> }>(
    AGENT_BASE,
    "/v1/dream-policies",
  );
}

export function createDreamPolicy(payload: Record<string, unknown>) {
  return request<{ dream_policy_id: string; version: number }>(AGENT_BASE, "/v1/dream-policies", {
    method: "POST",
    body: JSON.stringify(payload),
  });
}
