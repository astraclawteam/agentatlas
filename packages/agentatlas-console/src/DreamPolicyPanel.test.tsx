import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DreamPolicyPanel } from "./DreamPolicyPanel";
import type { DreamPolicyLifecycle } from "./api/dream";

const bindings = [{ handle: "opaque-binding", name: "企业梦境", version_label: "第 2 版", output_name: "研发知识" }];
describe("DreamPolicyPanel", () => {
  it("creates a governed draft without tickets or internal identifiers", async () => {
    const onSubmit = vi.fn(async (): Promise<DreamPolicyLifecycle> => ({ handle: "opaque-policy-handle", status: "draft", revision: 0, version: 0, permission_mode: "direct_edit", risk_reasons: [], cadence: "nightly", input_sources: ["work_brief"], visibility: "managers", confirmation: "high_risk_only", can_adopt: false }));
    render(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} bindings={bindings} onSubmit={onSubmit} />);
    fireEvent.click(screen.getByRole("button", { name: "保存梦境工作流" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    expect(await screen.findByRole("button", { name: "检查并提交复核" })).toBeVisible();
    expect(screen.queryByText(/X-Nexus-Ticket|票据|cron|timezone|workflow_id/i)).not.toBeInTheDocument();
  });
  it("shows the advanced bridge only with authorization and active mode", () => {
    const { rerender } = render(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} bindings={bindings} advancedAllowed advancedMode={false} />);
    expect(screen.queryByText("高级运行设置")).not.toBeInTheDocument();
    rerender(<DreamPolicyPanel organizations={[{ id: "dept", name: "研发一部" }]} bindings={bindings} advancedAllowed advancedMode />);
    expect(screen.getByText("高级运行设置")).toBeVisible();
    expect(screen.getByRole("link", { name: "打开高级工作流维护" })).toBeVisible();
  });
});
