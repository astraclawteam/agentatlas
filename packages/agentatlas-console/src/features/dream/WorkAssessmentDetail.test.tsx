import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { type WorkAssessmentDetail as WorkAssessmentDetailPayload } from "../../api/assessments";
import { WorkAssessmentDetail } from "./WorkAssessmentDetail";

afterEach(() => cleanup());

const managerPayload: WorkAssessmentDetailPayload = {
  subject: "actor-op-7",
  level: "individual",
  period: { start: "2026-06-15T00:00:00Z", end: "2026-07-14T00:00:00Z" },
  dimensions: [
    {
      dimension: "outcome_completion",
      state: "assessed",
      counted_outcome_keys: ["outcome-a", "outcome-b"],
      satisfied_outcomes: 2,
      confidence: { level: "high" },
      evidence: [{ tier: "verified_outcome", handle: "outcome-a", kind: "outcome" }],
      blockers: [{ handle: "blk-1", kind: "dependency", confidence: "verified", delay: "external" }],
    },
    { dimension: "quality", state: "not_assessable", not_assessable_reason: "insufficient_data" },
  ],
  narrative: "主管备注：整体评级 B+，详见校准记录",
  manager: { confirmed: true, manager: "mgr-line-lead" },
};

// A score-free payload (an employee-shaped projection): no satisfied_outcomes,
// no confidence, no narrative and no manager.
const employeePayload: WorkAssessmentDetailPayload = {
  subject: "actor-op-7",
  period: { start: "2026-06-15T00:00:00Z", end: "2026-07-14T00:00:00Z" },
  dimensions: [
    {
      dimension: "outcome_completion",
      counted_outcome_keys: ["outcome-a", "outcome-b"],
      evidence: [{ tier: "human_report", handle: "hr-1", kind: "report" }],
      blockers: [{ handle: "blk-1", kind: "dependency", confidence: "verified", delay: "external" }],
    },
    { dimension: "quality", state: "not_assessable", not_assessable_reason: "insufficient_data" },
  ],
};

describe("WorkAssessmentDetail", () => {
  it("renders the score, level and manager notes for an authorized manager payload", () => {
    render(<WorkAssessmentDetail assessment={managerPayload} />);
    // Facts.
    expect(screen.getByText("outcome_completion")).toBeVisible();
    // Score (satisfied outcomes) + level (confidence).
    expect(screen.getByText(/完成产出/)).toBeVisible();
    expect(screen.getByText(/置信度等级/)).toBeVisible();
    // Manager notes + confirming manager identity.
    expect(screen.getByText("主管备注：整体评级 B+，详见校准记录")).toBeVisible();
    expect(screen.getByText(/mgr-line-lead/)).toBeVisible();
  });

  it("does NOT render the score, level or manager notes for a score-free (employee) payload", () => {
    render(<WorkAssessmentDetail assessment={employeePayload} />);
    // Facts still render.
    expect(screen.getByText("outcome_completion")).toBeVisible();
    expect(screen.getByText(/数据不足/)).toBeVisible();
    // But no score, no level, no manager notes leak through the view.
    expect(screen.queryByText(/完成产出/)).not.toBeInTheDocument();
    expect(screen.queryByText(/置信度等级/)).not.toBeInTheDocument();
    expect(screen.queryByText(/主管备注/)).not.toBeInTheDocument();
    expect(screen.queryByText(/mgr-line-lead/)).not.toBeInTheDocument();
  });
});
