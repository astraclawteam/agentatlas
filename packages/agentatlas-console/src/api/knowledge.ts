import { api } from "./client";

export interface KnowledgeItem {
  key: string;
  title: string;
  type_label: string;
  updated_label: string;
  scope_label: string;
}

export interface KnowledgeHomeResponse {
  organization: { name: string };
  status: { knowledge_runtime: "running" | "checking"; freshness_label: string };
  counts: { available: boolean; recent_changes: number | null; reviews: number | null };
  items: KnowledgeItem[];
}

export function loadKnowledgeHome(orgUnitID: string, query: string, signal?: AbortSignal) {
  const params = new URLSearchParams({ org_unit_id: orgUnitID });
  const normalized = query.trim();
  if (normalized) params.set("query", normalized);
  return api<KnowledgeHomeResponse>(`/api/knowledge?${params.toString()}`, { signal });
}
