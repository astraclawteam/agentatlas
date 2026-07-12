import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ConsoleShell } from "../../app/ConsoleShell";
import type { Session } from "../../app/session";

const session: Session = {
  authenticated: true,
  enterprise_id: "ent-demo",
  enterprise_name: "示例企业",
  enterprise_user_id: "user-manager",
  display_name: "陈经理",
  org_version: 4,
  org_unit_ids: ["company", "dept-rd"],
  org_tree: [
    {
      id: "company",
      name: "全公司",
      selectable: true,
      children: [
        { id: "dept-rd", name: "研发一部", selectable: true, children: [] },
        { id: "dept-finance", name: "财务部", selectable: false, children: [] },
      ],
    },
  ],
  permissions: ["knowledge:read", "edit", "workflow_edit", "approve_high_risk"],
};

const knowledge = {
  organization: { name: "研发一部" },
  status: { running: true, freshness_label: "今天已更新" },
  counts: { recent_changes: 3, reviews: 2 },
  items: [
    { key: "item-1", title: "MES 异常工单处理", type_label: "SOP", updated_label: "昨天更新", scope_label: "研发一部" },
    { key: "item-2", title: "研发周报填写规范", type_label: "知识说明", updated_label: "7 天前更新", scope_label: "研发中心" },
  ],
};

function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

describe("KnowledgeHome", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(async (input) => {
      const url = String(input);
      if (url === "/api/session") return json(session);
      if (url.startsWith("/api/knowledge?")) return json(knowledge);
      return json({ message: "not found" }, 404);
    }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("explains the selected organization and presents four novice task shortcuts", async () => {
    render(<ConsoleShell initialPath="/knowledge/dept-rd" />);

    expect(await screen.findByRole("heading", { name: "研发一部的企业知识" })).toBeVisible();
    expect(screen.getByText("这些知识会通过企业网关提供给获得授权的员工 Agent")).toBeVisible();
    expect(screen.getByText("这里用于维护企业知识。可以补充资料、修正内容、制作 SOP，或处理员工建议。")).toBeVisible();
    for (const label of ["添加资料", "新建或修改知识", "制作 SOP 流程", "处理建议与审核"]) {
      expect(screen.getByRole("link", { name: new RegExp(label) })).toBeVisible();
    }
    expect(document.querySelectorAll('[data-variant="primary"]')).toHaveLength(1);
    expect(screen.queryByPlaceholderText(/票据|token|ID/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/provider|index|原始 JSON/i)).not.toBeInTheDocument();
  });

  it("shows only authorized organization links and reloads content when scope changes", async () => {
    render(<ConsoleShell initialPath="/knowledge/company" />);

    expect(await screen.findByRole("navigation", { name: "知识范围" })).toBeVisible();
    expect(screen.getByRole("link", { name: "全公司" })).toBeVisible();
    expect(screen.getByRole("link", { name: "研发一部" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "财务部" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("link", { name: "研发一部" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      expect.stringContaining("org_unit_id=dept-rd"),
      expect.objectContaining({ credentials: "include" }),
    ));
  });

  it("keeps search collapsed until requested and supports clear and Escape", async () => {
    render(<ConsoleShell initialPath="/knowledge/dept-rd" />);
    await screen.findByText("MES 异常工单处理");
    expect(screen.queryByRole("searchbox", { name: "搜索已有内容" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "搜索已有内容" }));
    const input = screen.getByRole("searchbox", { name: "搜索已有内容" });
    expect(input).toHaveFocus();
    fireEvent.change(input, { target: { value: "MES" } });
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      expect.stringContaining("query=MES"),
      expect.anything(),
    ));

    fireEvent.click(screen.getByRole("button", { name: "清除搜索" }));
    expect(input).toHaveValue("");
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      expect.not.stringContaining("query="),
      expect.anything(),
    ));
    fireEvent.keyDown(input, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("searchbox", { name: "搜索已有内容" })).not.toBeInTheDocument());
  });

  it("renders recent and review counts plus readable content rows", async () => {
    render(<ConsoleShell initialPath="/knowledge/dept-rd" />);

    expect(await screen.findByText("最近修改 3")).toBeVisible();
    expect(screen.getByText("需要我审核 2")).toBeVisible();
    expect(screen.getByText("MES 异常工单处理")).toBeVisible();
    expect(screen.getByText("SOP · 昨天更新 · 研发一部")).toBeVisible();
    expect(screen.getByRole("link", { name: "查看 MES 异常工单处理" })).toBeVisible();
  });

  it.each([
    ["empty", { ...knowledge, items: [] }, "这个范围还没有可用内容", "先添加一份资料，系统会帮助你整理成可用知识。"],
    ["error", { message: "temporary" }, "暂时无法读取企业知识", "你的内容没有丢失，请检查网络后重试。"],
  ])("provides a next step for the %s state", async (_name, response, title, next) => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      if (String(input) === "/api/session") return json(session);
      return _name === "error" ? json(response, 503) : json(response);
    });
    render(<ConsoleShell initialPath="/knowledge/dept-rd" />);
    expect(await screen.findByText(title)).toBeVisible();
    expect(screen.getByText(next)).toBeVisible();
    if (_name === "error") {
      fireEvent.click(screen.getByRole("button", { name: "重新读取" }));
      await waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
    }
  });

  it("announces a truthful loading state", async () => {
    let resolveKnowledge!: (response: Response) => void;
    vi.mocked(fetch).mockImplementation((input) => {
      if (String(input) === "/api/session") return Promise.resolve(json(session));
      return new Promise<Response>((resolve) => { resolveKnowledge = resolve; });
    });
    render(<ConsoleShell initialPath="/knowledge/dept-rd" />);
    expect(await screen.findByText("正在读取研发一部的知识…")).toBeVisible();
    resolveKnowledge(json(knowledge));
    expect(await screen.findByText("MES 异常工单处理")).toBeVisible();
  });
});
