import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ConsoleShell } from "../../app/ConsoleShell";
import type { Session } from "../../app/session";

const editorSession: Session = {
  authenticated: true,
  enterprise_id: "ent-demo",
  enterprise_name: "示例企业",
  enterprise_user_id: "editor-user",
  display_name: "陈经理",
  org_version: 4,
  org_unit_ids: ["dept-rd"],
  org_tree: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }],
  permissions: ["suggest", "edit", "workflow_edit", "publish_low_risk"],
  advanced_mode_allowed: true,
};

const initialDraft = {
  change_id: "change-1",
  enterprise_id: "ent-demo",
  org_unit_id: "dept-rd",
  resource_type: "knowledge_entry",
  resource_id: "knowledge-new",
  action: "update",
  requester_user_id: "editor-user",
  origin: "direct_edit",
  permission_mode: "direct_edit",
  revision: 1,
  state: "draft",
  base_version: 0,
  proposed_content: { title: "MES 处理", summary: "原说明", sections: [{ heading: "处理方法", body: "先检查工单" }], references: [] },
  created_at: "2026-07-13T01:00:00Z",
  updated_at: "2026-07-13T01:00:00Z",
};

function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

describe("KnowledgeEditor", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("routes the knowledge shortcut to a structured organization-scoped editor", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input) => String(input) === "/api/session" ? json(editorSession) : json({ message: "not found" }, 404)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/edit" />);

    expect(await screen.findByRole("heading", { name: "新建或修改知识" })).toBeVisible();
    expect(screen.getByRole("navigation", { name: "内容目录" })).toBeVisible();
    expect(screen.getByText("当前范围：研发一部")).toBeVisible();
    expect(screen.getByText("影响范围")).toBeVisible();
    expect(screen.getByText("参考资料")).toBeVisible();
    expect(screen.queryByText(/resource_id|proposed_content|原始 JSON|knowledge_entry/)).not.toBeInTheDocument();
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(1);
  });

  it("edits SOPs as numbered steps with explicit ordering controls", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input) => String(input) === "/api/session" ? json(editorSession) : json({ message: "not found" }, 404)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/sop" />);
    expect(await screen.findByRole("heading", { name: "制作 SOP 流程" })).toBeVisible();
    fireEvent.change(screen.getByRole("textbox", { name: "第 1 步名称" }), { target: { value: "检查工单" } });
    fireEvent.click(screen.getByRole("button", { name: "添加一步" }));
    fireEvent.change(screen.getByRole("textbox", { name: "第 2 步名称" }), { target: { value: "通知负责人" } });
    fireEvent.click(screen.getByRole("button", { name: "上移第 2 步" }));
    expect(screen.getByRole("textbox", { name: "第 1 步名称" })).toHaveValue("通知负责人");
    expect(screen.getByRole("textbox", { name: "第 2 步名称" })).toHaveValue("检查工单");
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(1);
  });

  it("waits 800ms, reports truthful autosave states, and warns only while unsaved", async () => {
    let resolveCreate!: (response: Response) => void;
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input) === "/api/session") return Promise.resolve(json(editorSession));
      if (String(input) === "/api/changes") return new Promise<Response>((resolve) => { resolveCreate = resolve; });
      return Promise.resolve(json({ message: "not found" }, 404));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/edit" />);

    const title = await screen.findByRole("textbox", { name: "知识名称" });
    expect(screen.getByText("尚未保存")).toBeVisible();
    fireEvent.change(title, { target: { value: "MES 处理" } });
    const unload = new Event("beforeunload", { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);

    await act(async () => { vi.advanceTimersByTime(799); });
    expect(fetchMock).not.toHaveBeenCalledWith("/api/changes", expect.anything());
    await act(async () => { vi.advanceTimersByTime(1); });
    expect(screen.getByText("正在保存")).toBeVisible();
    expect(fetchMock).toHaveBeenCalledWith("/api/changes", expect.objectContaining({ credentials: "include", method: "POST" }));

    await act(async () => { resolveCreate(json(initialDraft, 201)); });
    expect(await screen.findByText(/^已保存 \d{2}:\d{2}$/)).toBeVisible();
    const savedUnload = new Event("beforeunload", { cancelable: true }) as BeforeUnloadEvent;
    window.dispatchEvent(savedUnload);
    expect(savedUnload.defaultPrevented).toBe(false);
    expect(screen.queryByText(/保存中|自动保存成功/)).not.toBeInTheDocument();
  });

  it("keeps the in-memory draft after save failure and retries explicitly", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input) === "/api/session") return json(editorSession);
      if (String(input) === "/api/changes") return json({ message: "网络暂时不可用" }, 503);
      return json({ message: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/edit" />);
    const title = await screen.findByRole("textbox", { name: "知识名称" });
    fireEvent.change(title, { target: { value: "不能丢失的草稿" } });
    await act(async () => { vi.advanceTimersByTime(800); });

    expect(await screen.findByRole("button", { name: "保存失败，重试" })).toBeVisible();
    expect(title).toHaveValue("不能丢失的草稿");
    expect(localStorage.length).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: "保存失败，重试" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  });

  it("never reports a newer edit as saved by an older request", async () => {
    let resolveFirst!: (response: Response) => void;
    const bodies: string[] = [];
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === "/api/session") return Promise.resolve(json(editorSession));
      if (String(input) === "/api/changes") {
        bodies.push(String(init?.body));
        if (bodies.length === 1) return new Promise<Response>((resolve) => { resolveFirst = resolve; });
        return Promise.resolve(json({ ...initialDraft, revision: 2, proposed_content: JSON.parse(bodies.at(-1)!).proposed_content }));
      }
      if (String(input) === "/api/changes/change-1" && init?.method === "PUT") {
        bodies.push(String(init.body));
        return Promise.resolve(json({ ...initialDraft, revision: 2, proposed_content: JSON.parse(String(init.body)).proposed_content }));
      }
      if (String(input) === "/api/changes/change-1") return Promise.resolve(json({ draft: initialDraft, content: contentFromBody(bodies.at(-1)), base_content: {} }));
      return Promise.resolve(json({ message: "not found" }, 404));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/edit" />);
    const title = await screen.findByRole("textbox", { name: "知识名称" });
    fireEvent.change(title, { target: { value: "第一版" } });
    await act(async () => { vi.advanceTimersByTime(800); });
    expect(screen.getByText("正在保存")).toBeVisible();
    fireEvent.change(title, { target: { value: "第二版" } });
    await act(async () => { resolveFirst(json({ ...initialDraft, proposed_content: { ...initialDraft.proposed_content, title: "第一版" } }, 201)); });

    expect(screen.getByText("尚未保存")).toBeVisible();
    expect(title).toHaveValue("第二版");
    await act(async () => { vi.advanceTimersByTime(800); });
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(JSON.parse(bodies[1]).proposed_content.title).toBe("第二版");
  });

  it("shows both versions on 409 and reapplies only after explicit confirmation", async () => {
    const conflict = {
      error: "revision_conflict",
      current_revision: 2,
      diff: {
        before: { ...initialDraft.proposed_content, summary: "同事刚保存的说明" },
        after: { ...initialDraft.proposed_content, summary: "我的说明" },
      },
    };
    const calls: Array<{ url: string; body?: string }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, body: init?.body as string | undefined });
      if (url === "/api/session") return json(editorSession);
      if (url === "/api/changes/change-1") {
        if (init?.method === "PUT") {
          const updates = calls.filter((call) => call.url === url && call.body);
          return updates.length === 1 ? json(conflict, 409) : json({ ...initialDraft, revision: 3, proposed_content: conflict.diff.after });
        }
        return json({ draft: initialDraft, content: initialDraft.proposed_content, base_content: {} });
      }
      return json({ message: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/edit" />);

    const summary = await screen.findByRole("textbox", { name: "简要说明" });
    fireEvent.change(summary, { target: { value: "我的说明" } });
    await act(async () => { vi.advanceTimersByTime(800); });
    expect(await screen.findByRole("heading", { name: "发现其他人的新修改" })).toBeVisible();
    expect(screen.getByText("同事刚保存的说明")).toBeVisible();
    expect(screen.getByText("我的说明")).toBeVisible();
    expect(calls.filter((call) => call.url === "/api/changes/change-1" && call.body)).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "重新应用我的修改" }));
    await waitFor(() => expect(calls.filter((call) => call.url === "/api/changes/change-1" && call.body)).toHaveLength(2));
    expect(JSON.parse(calls.at(-1)!.body!).revision).toBe(2);
  });

  it("lets suggestion-only employees submit a suggestion but never mutate or publish", async () => {
    const employee = { ...editorSession, enterprise_user_id: "employee", display_name: "李员工", permissions: ["suggest"] };
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === "/api/session") return json(employee);
      if (String(input) === "/api/changes/suggestions") return json({ ...initialDraft, origin: "employee_suggestion", permission_mode: "suggestion_only" }, 201);
      return json({ message: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/edit" />);
    fireEvent.change(await screen.findByRole("textbox", { name: "知识名称" }), { target: { value: "修正建议" } });
    fireEvent.change(screen.getByRole("textbox", { name: "简要说明" }), { target: { value: "这里需要修正" } });
    fireEvent.click(screen.getByRole("button", { name: "提交纠错建议" }));

    expect(await screen.findByText("建议已提交给知识负责人")).toBeVisible();
    expect(fetchMock).toHaveBeenCalledWith("/api/changes/suggestions", expect.objectContaining({ method: "POST", credentials: "include" }));
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringMatching(/decisions|publish/), expect.anything());
  });
});

function contentFromBody(body: string | undefined) {
  return body ? JSON.parse(body).proposed_content : initialDraft.proposed_content;
}
