import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DreamPolicyPanel } from "./DreamPolicyPanel";
import type { DreamPolicyLifecycle } from "./api/dream";

describe("DreamPolicyPanel", () => {
  it("creates a governed draft without tickets or direct publish", async () => {
    const onSubmit = vi.fn(async (): Promise<DreamPolicyLifecycle> => ({
      dream_policy_id: "hidden-policy", status: "draft", revision: 0, version: 0,
      requester_user_id: "manager", permission_mode: "direct_edit", risk_reasons: [], org_path: [],
      policy: { org_unit_id: "dept", timezone: "Asia/Shanghai", schedule: "0 22 * * *", input_sources: ["work_brief"], workflow: { id: "hidden-workflow", version: 2 }, output_space_id: "hidden-space", visibility_level: "managers", masking_rules: [], risk_signal_rules: [], evidence_retention: "pointer_only", confirmation_mode: "high_risk_only", max_attempts: 3, allow_partial_children: false },
    }));
    render(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} currentUserID="manager" onSubmit={onSubmit} />);
    fireEvent.click(screen.getByRole("button", { name: "保存梦境工作流" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    expect(await screen.findByRole("button", { name: "检查并提交复核" })).toBeVisible();
    expect(screen.queryByText(/X-Nexus-Ticket|票据|hidden-policy|hidden-workflow|cron/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /直接发布/ })).not.toBeInTheDocument();
  });

  it("shows advanced fields only with authorization and active mode", () => {
    const { rerender } = render(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} advancedAllowed advancedMode={false} />);
    expect(screen.queryByText("高级运行设置")).not.toBeInTheDocument();
    rerender(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} advancedAllowed advancedMode />);
    expect(screen.getByText("高级运行设置")).toBeVisible();
  });
});
