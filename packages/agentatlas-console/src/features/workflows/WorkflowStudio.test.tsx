import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import type { AtlasWorkflow } from "../../types";
import { WorkflowStudio, type WorkflowDraftRecord, type WorkflowStudioRepository } from "./WorkflowStudio";

const linear: AtlasWorkflow = {
  workflow_id: "wf-1", version: 0, kind: "sop", risk_level: "low",
  nodes: [{ id: "a", type: "input.manual", name: "收到申请" }, { id: "b", type: "human.confirm", name: "主管确认" }],
  edges: [{ from: "a", to: "b" }],
};

function record(overrides: Partial<WorkflowDraftRecord> = {}): WorkflowDraftRecord {
  return { handle: "opaque-draft", name: "请假审批", status: "draft", revision: 3, definition: linear, can_edit: true, ...overrides };
}

function repository(overrides: Partial<WorkflowStudioRepository> = {}): WorkflowStudioRepository {
  return {
    updateDraft: vi.fn(async (_handle, revision, definition) => record({ revision: revision + 1, definition })),
    createDraftFromPublished: vi.fn(async () => record()),
    prepareReview: vi.fn(async () => ({ change_id: "change-1" })),
    ...overrides,
  };
}

function renderStudio(item: WorkflowDraftRecord, repo = repository(), advanced = false) {
  render(<MemoryRouter><WorkflowStudio item={item} repository={repo} orgUnitID="dept-rd" advancedMode={advanced} permissions={["workflow_edit", "workflow_advanced"]} /></MemoryRouter>);
  return repo;
}

describe("workflow editing", () => {
  it("updates the same draft using its current revision", async () => {
    const repo = renderStudio(record());
    fireEvent.click(screen.getByRole("button", { name: "下移 主管确认" }));
    fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
    await waitFor(() => expect(repo.updateDraft).toHaveBeenCalledWith("opaque-draft", 3, expect.any(Object)));
    expect(repo.createDraftFromPublished).not.toHaveBeenCalled();
  });

  it("creates one draft from a published version, then updates that draft", async () => {
    const repo = renderStudio(record({ status: "published", revision: 0, handle: "published-handle" }));
    fireEvent.click(screen.getByRole("button", { name: "开始修改" }));
    await waitFor(() => expect(repo.createDraftFromPublished).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
    await waitFor(() => expect(repo.updateDraft).toHaveBeenCalledWith("opaque-draft", 3, expect.any(Object)));
    expect(repo.createDraftFromPublished).toHaveBeenCalledTimes(1);
  });

  it("shows a truthful comparison action on an optimistic conflict", async () => {
    const latest = record({ revision: 4, definition: { ...linear, risk_level: "high" } });
    const repo = repository({ updateDraft: vi.fn(async () => { throw Object.assign(new Error("conflict"), { status: 409, details: { latest } }); }) });
    renderStudio(record(), repo);
    fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
    expect(await screen.findByText("其他人刚刚保存了新版本。你的修改尚未覆盖服务器内容。" )).toBeVisible();
    expect(screen.getByRole("button", { name: "比较并重新应用" })).toBeVisible();
  });

  it("keeps branched workflows read-only in Basic mode and never exposes English node types", () => {
    const branched = { ...linear, nodes: [...linear.nodes, { id: "c", type: "custom.secret", name: "系统校验" }], edges: [{ from: "a", to: "b" }, { from: "a", to: "c" }] } as AtlasWorkflow;
    renderStudio(record({ definition: branched }));
    expect(screen.getByText("这个流程包含分支或高级设置，基础模式只能查看。" )).toBeVisible();
    expect(screen.queryByText("custom.secret")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "使用高级维护模式打开" })).toBeVisible();
  });

  it("requires permission and an explicitly enabled Advanced mode before rendering FlowGram", () => {
    const branched = { ...linear, edges: [{ from: "a", to: "b", condition: "approved" }] };
    const { rerender } = render(<MemoryRouter><WorkflowStudio item={record({ definition: branched })} repository={repository()} orgUnitID="dept-rd" advancedMode={false} permissions={["workflow_edit", "workflow_advanced"]} /></MemoryRouter>);
    expect(screen.queryByTestId("advanced-workflow-studio")).not.toBeInTheDocument();
    rerender(<MemoryRouter><WorkflowStudio item={record({ definition: branched })} repository={repository()} orgUnitID="dept-rd" advancedMode permissions={["workflow_edit", "workflow_advanced"]} /></MemoryRouter>);
    expect(screen.getByTestId("advanced-workflow-studio")).toBeVisible();
  });

  it("routes publication through the shared governed review", async () => {
    const repo = renderStudio(record());
    fireEvent.click(screen.getByRole("button", { name: "下一步：检查并发布" }));
    await waitFor(() => expect(repo.prepareReview).toHaveBeenCalledWith("opaque-draft", "dept-rd", 3));
    expect(screen.queryByRole("button", { name: /^发布$/ })).not.toBeInTheDocument();
  });
});
