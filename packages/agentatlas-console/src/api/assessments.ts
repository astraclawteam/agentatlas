import { api } from "./client";

// assessments.ts is the management-BFF client for the Task 18D manager
// assessment detail (GET /api/assessments/{id}). The backend returns the
// authorized detail ONLY for a subject within the manager's exact hierarchy
// scope and under a policy they own; an unauthorized read is a 403/404 that the
// shared api<T>() helper surfaces as an ApiError. Employee delivery stays
// Xiaozhi-only and is NOT a Console surface.

export type AssessmentDimensionState = "assessed" | "not_assessable";
export type AssessmentConfidenceLevel = "high" | "medium" | "low";

export interface AssessmentEvidenceRef {
  tier: "verified_outcome" | "accepted_deliverable" | "human_report";
  handle: string;
  kind: string;
  summary?: string;
}

export interface AssessmentBlocker {
  handle: string;
  kind: string;
  confidence: "verified" | "corroborated" | "reported" | "inferred";
  delay: "external" | "personal" | "process" | "resource" | "unattributed";
  summary?: string;
}

export interface AssessmentConfidence {
  level: AssessmentConfidenceLevel;
  components?: { kind: string; score: number }[];
}

// AssessmentDimensionDetail mirrors the frozen sdk/go/assessment DimensionResult.
// The SCORE (satisfied_outcomes) and the LEVEL (confidence) are present ONLY on
// an authorized manager payload; an employee-shaped payload omits them.
export interface AssessmentDimensionDetail {
  dimension: string;
  state?: AssessmentDimensionState;
  not_assessable_reason?: string;
  counted_outcome_keys?: string[];
  satisfied_outcomes?: number;
  evidence?: AssessmentEvidenceRef[];
  blockers?: AssessmentBlocker[];
  confidence?: AssessmentConfidence;
}

export interface AssessmentManagerConfirmation {
  confirmed: boolean;
  manager?: string;
  confirmed_at?: string;
}

// WorkAssessmentDetail mirrors the frozen sdk/go/assessment WorkAssessment. The
// manager-only fields (satisfied_outcomes/confidence per dimension, narrative and
// manager confirmation) are optional so the same type can also describe a
// score-free payload the detail view must render WITHOUT any score/level/notes.
export interface WorkAssessmentDetail {
  id?: string;
  subject: string;
  level?: string;
  policy_key?: string;
  policy_revision?: number;
  version?: number;
  period: { start: string; end: string };
  dimensions: AssessmentDimensionDetail[];
  narrative?: string;
  manager?: AssessmentManagerConfirmation;
}

export interface ManagerAssessmentResponse {
  assessment: WorkAssessmentDetail;
}

// getManagerAssessment fetches the authorized manager assessment detail. It is
// reached from existing work-memory/Dream context or the Atlas Assistant, never
// through a new navigation surface (the Console layout is frozen).
export function getManagerAssessment(id: string, signal?: AbortSignal) {
  return api<ManagerAssessmentResponse>(`/api/assessments/${encodeURIComponent(id)}`, { signal });
}
