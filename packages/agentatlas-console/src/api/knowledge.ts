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
  status: { running: boolean; freshness_label: string };
  counts: { recent_changes: number; reviews: number };
  items: KnowledgeItem[];
}

export function loadKnowledgeHome(orgUnitID: string, query: string) {
  const params = new URLSearchParams({ org_unit_id: orgUnitID });
  const normalized = query.trim();
  if (normalized) params.set("query", normalized);
  return api<KnowledgeHomeResponse>(`/api/knowledge?${params.toString()}`);
}
