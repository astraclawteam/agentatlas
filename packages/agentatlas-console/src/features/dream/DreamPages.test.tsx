import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  type BasicDreamPolicy,
  type DreamHierarchyNode,
  type DreamRun,
  type DreamPolicyLifecycle,
} from "../../api/dream";
import { DreamOverviewPage } from "./DreamOverviewPage";
import { DreamPolicyWizard } from "./DreamPolicyWizard";
import { DreamRunDetailPage } from "./DreamRunDetailPage";
import { DreamTimeline } from "../../DreamTimeline";
import { DreamPolicyLifecycleControls } from "../../DreamPolicyPanel";

const run: DreamRun = {
  handle: "opaque-run-handle-abcdefghijklmnopqrstuvwxyz",
  organization_id: "dept-rd",
  status: "succeeded",
  window_start: "2026-07-11T14:00:00Z",
  window_end: "2026-07-12T14:00:00Z",
  input_count: 12,
  coverage: { expected_children: 3, completed_children: 2, input_count: 12 },
  missing_input_reasons: ["下级组织尚未完成整理"],
  facts: [{ title: "交付稳定", detail: "本周完成两个里程碑", severity: "info" }],
  themes: [],
  trends: [{ title: "响应更快", detail: "平均处理时间下降", severity: "info" }],
  risks: [{ title: "测试覆盖不足", detail: "一个项目组尚未完成", severity: "warning" }],
  todos: [{ title: "补齐测试记录", detail: "由研发二组跟进", severity: "warning" }],
  display_summary: "研发一部本周交付稳定，但一个下级组织的输入尚未完成。",
  rerun: false,
  input_organizations: [{ organization_name: "研发二组", relation: "下级组织汇总" }],
  annotations: [],
};

const hierarchy: DreamHierarchyNode[] = [{
  org: { id: "company", name: "全公司", selectable: true },
  run: { ...run, organization_id: "company", coverage: { expected_children: 1, completed_children: 1, input_count: 12 }, missing_input_reasons: [], risks: [] },
  children: [{
    org: { id: "dept-rd", name: "研发事业部", selectable: true },
    run,
    children: [],
  }],
}];

describe("enterprise Dream pages", () => {
  afterEach(() => vi.restoreAllMocks());

  it("renders a readable organization hierarchy with partial coverage and no raw identifiers", () => {
    render(<DreamOverviewPage data={hierarchy} />);
    expect(screen.getByRole("heading", { name: "梦境全景" })).toBeVisible();
    expect(screen.getByText("全公司")).toBeVisible();
    expect(screen.getByText("研发事业部")).toBeVisible();
    expect(screen.getByText("覆盖 2/3 个下级组织")).toBeVisible();
    expect(screen.getByText("有 1 项输入未完成")).toBeVisible();
    expect(screen.getByText("需要留意")).toBeVisible();
    expect(screen.queryByText(/run-secret|pointer-secret|workflow-secret/)).not.toBeInTheDocument();
  });

  it("filters timeline by organization and window and explains structured results", () => {
    render(<DreamTimeline runs={[run, { ...run, handle: "opaque-other-handle", organization_id: "company", display_summary: "公司摘要" }]} organizations={[{ id: "dept-rd", name: "研发事业部" }, { id: "company", name: "全公司" }]} />);
    fireEvent.change(screen.getByLabelText("选择组织"), { target: { value: "dept-rd" } });
    expect(screen.getByText(run.display_summary)).toBeVisible();
    expect(screen.getByText("响应更快")).toBeVisible();
    expect(screen.getByText("测试覆盖不足")).toBeVisible();
    expect(screen.getByText("补齐测试记录")).toBeVisible();
    expect(screen.getByText("等待补齐输入")).toBeVisible();
    expect(screen.queryByText("公司摘要")).not.toBeInTheDocument();
  });

  it("maps the basic wizard to public policy fields without showing cron, timezone, masking, or IDs", () => {
    const onSubmit = vi.fn();
    render(<DreamPolicyWizard organizations={[{ id: "dept-rd", name: "研发事业部" }]} bindings={[{ handle: "opaque-binding", name: "企业梦境整理", version_label: "第 2 版", output_name: "研发知识" }]} onSubmit={onSubmit} />);
    for (const label of ["整理哪个组织", "多久整理一次", "使用哪些记录", "谁能看到", "是否需要确认"]) {
      expect(screen.getByText(label)).toBeVisible();
    }
    expect(screen.queryByText(/cron|timezone|时区|遮罩|ID/i)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "保存梦境工作流" }));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ organization: "dept-rd", cadence: "nightly" }));
    const submitted = onSubmit.mock.calls[0]?.[0] as BasicDreamPolicy;
    expect(submitted).toEqual(expect.objectContaining({ organization: "dept-rd", cadence: "nightly" }));
  });

  it("shows advanced diagnostics and pinned workflow only for authorized active Advanced mode", () => {
    const props = { organizations: [{ id: "dept-rd", name: "研发事业部" }], bindings: [{ handle: "opaque-binding", name: "企业梦境整理", version_label: "第 2 版", output_name: "研发知识" }] };
    const { rerender } = render(<DreamPolicyWizard {...props} advancedAllowed advancedMode={false} />);
    expect(screen.queryByText("高级运行设置")).not.toBeInTheDocument();
    rerender(<DreamPolicyWizard {...props} advancedAllowed advancedMode />);
    expect(screen.getByText("高级运行设置")).toBeVisible();
    expect(screen.getByRole("link", { name: "打开高级工作流维护" })).toBeVisible();
  });

  it("drives governed policy review without direct publish or self-review", () => {
    const submit = vi.fn(); const decide = vi.fn(); const publish = vi.fn(); const backfill = vi.fn();
    const base: DreamPolicyLifecycle = { handle: "opaque-policy-handle", status: "draft", revision: 0, version: 0, permission_mode: "direct_edit", risk_reasons: [], cadence: "nightly", input_sources: ["work_brief"], visibility: "managers", confirmation: "high_risk_only", can_adopt: false };
    const { rerender } = render(<DreamPolicyLifecycleControls policy={base} currentUserID="creator" onSubmitReview={submit} onDecide={decide} onPublish={publish} />);
    expect(screen.getByRole("button", { name: "检查并提交复核" })).toBeVisible();
    expect(screen.queryByRole("button", { name: /直接发布/ })).not.toBeInTheDocument();
    rerender(<DreamPolicyLifecycleControls policy={{ ...base, status: "review_pending", risk_level: "high", review_mode: "upward_review", review_state: "pending", pending_action: "publish", can_decide: false }} currentUserID="creator" onSubmitReview={submit} onDecide={decide} onPublish={publish} />);
    expect(screen.getByText("已提交给上级负责人复核")).toBeVisible();
    expect(screen.queryByRole("button", { name: "批准发布" })).not.toBeInTheDocument();
    rerender(<DreamPolicyLifecycleControls policy={{ ...base, status: "review_pending", risk_level: "high", review_mode: "upward_review", review_state: "pending", pending_action: "publish", can_decide: true }} currentUserID="manager" onSubmitReview={submit} onDecide={decide} onPublish={publish} />);
    fireEvent.click(screen.getByRole("button", { name: "批准发布" }));
    expect(decide).toHaveBeenCalledWith("approve");
    rerender(<DreamPolicyLifecycleControls policy={{ ...base, status: "approved", risk_level: "high", review_state: "approved", pending_action: "publish" }} currentUserID="creator" onSubmitReview={submit} onDecide={decide} onPublish={publish} />);
    fireEvent.click(screen.getByRole("button", { name: "发布新版本" }));
    expect(publish).toHaveBeenCalled();
    rerender(<DreamPolicyLifecycleControls policy={{ ...base, status: "published", version: 2 }} currentUserID="creator" advancedMode onSubmitReview={submit} onDecide={decide} onPublish={publish} onBackfill={backfill} />);
    expect(screen.getByLabelText("补跑开始时间")).toBeVisible();
    expect(screen.getByRole("button", { name: "补跑所选时间段" })).toBeVisible();
  });

  it("keeps historical output immutable and appends governed annotations and an idempotent rerun", async () => {
    const annotate = vi.fn(async () => undefined);
    const rerun = vi.fn(async (_key: string) => ({ handle: "new-opaque-handle" }));
    render(<DreamRunDetailPage run={run} organizationName="研发事业部" onAnnotate={annotate} onRerun={rerun} />);
    expect(screen.queryByRole("button", { name: /编辑摘要/ })).not.toBeInTheDocument();
    expect(screen.getByText("这是历史运行结果，不能直接修改")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "标记结果有误" }));
    await waitFor(() => expect(annotate).toHaveBeenCalledWith("mark_incorrect", ""));
    fireEvent.click(screen.getByRole("button", { name: "重新整理这个时间段" }));
    await waitFor(() => expect(rerun).toHaveBeenCalledTimes(1));
    expect(rerun.mock.calls[0][0]).toMatch(/^atlas-rerun-/);
  });

  it("requests evidence access first, explains denial, and only reveals sanitized allowed detail", async () => {
    const requestEvidence = vi.fn()
      .mockRejectedValueOnce(Object.assign(new Error("denied"), { status: 403 }))
      .mockResolvedValueOnce({ sanitized_detail: "已脱敏：测试记录显示两个项目已完成。" });
    const { rerender } = render(<DreamRunDetailPage run={run} organizationName="研发事业部" onEvidenceAccess={requestEvidence} />);
    fireEvent.click(screen.getByRole("button", { name: "申请查看原始依据" }));
    expect(await screen.findByText("当前没有查看权限。请联系该组织的负责人授权后再试。")).toBeVisible();
    expect(screen.queryByText(/pointer-secret|grant|audit/i)).not.toBeInTheDocument();
    rerender(<DreamRunDetailPage run={run} organizationName="研发事业部" onEvidenceAccess={requestEvidence} />);
    fireEvent.click(screen.getByRole("button", { name: "申请查看原始依据" }));
    expect(await screen.findByText("已脱敏：测试记录显示两个项目已完成。")).toBeVisible();
    expect(requestEvidence).toHaveBeenCalledTimes(2);
  });

  it.each(["loading", "error", "empty"] as const)("renders a truthful %s overview state with a next step", (state) => {
    render(<DreamOverviewPage state={state} data={[]} />);
    expect(screen.getByTestId(`dream-overview-${state}`)).toBeVisible();
    expect(screen.getByText(/^下一步：/)).toBeVisible();
  });
});
