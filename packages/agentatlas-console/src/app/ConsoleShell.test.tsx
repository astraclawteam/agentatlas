import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ConsoleShell } from "./ConsoleShell";
import type { Session } from "./session";

const managerSession: Session = {
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
      children: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }],
    },
  ],
  permissions: ["knowledge:read", "knowledge:write"],
  advanced_mode_allowed: true,
};

function sessionResponse(session = managerSession) {
  return new Response(JSON.stringify(session), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("ConsoleShell", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(async (input) => {
      const url = String(input);
      if (url === "/api/session") return sessionResponse();
      if (url.startsWith("/api/knowledge?")) {
        return new Response(JSON.stringify({
          organization: { name: "\u7814\u53d1\u4e00\u90e8" },
          status: { knowledge_runtime: "running", freshness_label: "today" },
          counts: { available: true, recent_changes: 0, reviews: 0 },
          items: [],
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      return new Response(null, { status: 404 });
    }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("redirects the full page with the active route when session is absent", async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 401 }));
    const assignLocation = vi.fn();

    render(<ConsoleShell initialPath="/knowledge/dept-rd?review=pending" assignLocation={assignLocation} />);

    await waitFor(() =>
      expect(assignLocation).toHaveBeenCalledWith(
        "/auth/login?return_to=%2Fknowledge%2Fdept-rd%3Freview%3Dpending",
      ),
    );
    expect(fetch).toHaveBeenCalledWith(
      "/api/session",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("restores the requested route and renders the four product surfaces", async () => {
    render(<ConsoleShell initialPath="/dream/timeline?window=week" />);

    expect(await screen.findByRole("heading", { name: "梦境时间线" })).toBeVisible();
    const navigation = screen.getByRole("navigation", { name: "主要工作区" });
    for (const label of ["企业知识", "企业梦境", "做事流程", "回答依据"]) {
      expect(withinNavigation(navigation, label)).toBeVisible();
    }
  });

  it("keeps the Agent button immediately left of aligned no-wrap user controls", async () => {
    render(<ConsoleShell initialPath="/knowledge" />);

    const actions = await screen.findByTestId("header-user-actions");
    expect(actions).toHaveClass("items-center", "whitespace-nowrap");
    expect(actions).toHaveAttribute("data-control-height", "48");
    expect(actions.children[0]).toBe(screen.getByRole("button", { name: "打开 Atlas 助手" }));
    expect(actions.children[1]).toHaveTextContent("陈经理");
  });

  it("docks the assistant in layout and returns focus on Escape", async () => {
    render(<ConsoleShell initialPath="/knowledge" />);
    const opener = await screen.findByRole("button", { name: "打开 Atlas 助手" });

    fireEvent.click(opener);
    const layout = screen.getByTestId("console-content-layout");
    expect(layout).toHaveAttribute("data-assistant-open", "true");
    expect(screen.getByRole("complementary", { name: "Atlas 助手" })).toHaveAttribute(
      "data-layout",
      "docked",
    );

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Atlas 助手" })).not.toBeInTheDocument());
    expect(opener).toHaveFocus();
  });

  it("shows Advanced Maintenance only to authorized sessions and keeps it in memory", async () => {
    const { unmount } = render(<ConsoleShell initialPath="/knowledge" />);
    const toggle = await screen.findByRole("checkbox", { name: "高级维护模式" });
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);
    expect(toggle).toBeChecked();
    unmount();

    vi.mocked(fetch).mockResolvedValue(
      sessionResponse({ ...managerSession, advanced_mode_allowed: false }),
    );
    render(<ConsoleShell initialPath="/knowledge" />);
    await screen.findByText("陈经理");
    expect(screen.queryByRole("checkbox", { name: "高级维护模式" })).not.toBeInTheDocument();
  });

  it("renders only authorized organization scopes as keyboard links", async () => {
    render(<ConsoleShell initialPath="/knowledge" />);

    expect(await screen.findByRole("navigation", { name: "知识范围" })).toBeVisible();
    expect(screen.getByRole("link", { name: "全公司" })).toHaveAttribute("href", "/knowledge/company");
    expect(screen.getByRole("link", { name: "研发一部" })).toHaveAttribute("href", "/knowledge/dept-rd");
    expect(screen.queryByText("财务部")).not.toBeInTheDocument();
  });

  it("keeps an unauthorized ancestor as non-focusable context", async () => {
    vi.mocked(fetch).mockResolvedValue(
      sessionResponse({
        ...managerSession,
        org_unit_ids: ["dept-rd"],
        org_tree: [
          {
            id: "company",
            name: "全公司",
            selectable: false,
            children: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }],
          },
        ],
      }),
    );

    render(<ConsoleShell initialPath="/knowledge" />);

    expect(await screen.findByText("全公司")).toBeVisible();
    expect(screen.queryByRole("link", { name: "全公司" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "研发一部" })).toBeVisible();
  });

  it("never renders a raw organization id when its presentation name is unavailable", async () => {
    vi.mocked(fetch).mockResolvedValue(
      sessionResponse({
        ...managerSession,
        org_unit_ids: ["department:opaque-7f31"],
        org_tree: [
          {
            id: "department:opaque-7f31",
            name: "未命名组织",
            selectable: false,
            children: [],
          },
        ],
      }),
    );

    render(<ConsoleShell initialPath="/knowledge" />);

    expect(await screen.findByText("未命名组织")).toBeVisible();
    expect(screen.queryByText("department:opaque-7f31")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "未命名组织" })).not.toBeInTheDocument();
  });

  it.each([
    ["knowledge", "旧版知识维护"],
    ["dream", "旧版梦境维护"],
    ["evidence", "旧版回答依据"],
  ])("keeps the authorized advanced %s capability reachable", async (surface, heading) => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      const url = String(input);
      if (url === "/api/session") return sessionResponse();
      if (url === `/api/legacy/${surface}`) {
        return new Response(JSON.stringify({ items: [{ id: "item-1", label: "可用内容" }] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(null, { status: 404 });
    });

    render(<ConsoleShell initialPath={`/advanced/legacy/${surface}`} />);

    expect(await screen.findByText("请先开启高级维护模式")).toBeVisible();
    expect(fetch).not.toHaveBeenCalledWith(`/api/legacy/${surface}`, expect.anything());
    fireEvent.click(screen.getByRole("checkbox", { name: "高级维护模式" }));
    expect(await screen.findByRole("heading", { name: heading })).toBeVisible();
    expect(await screen.findByText("可用内容")).toBeVisible();
    expect(screen.getByRole("link", { name: "旧版知识维护" })).toBeVisible();
    expect(screen.getByRole("link", { name: "旧版梦境维护" })).toBeVisible();
    expect(screen.getByRole("link", { name: "旧版流程维护" })).toBeVisible();
    expect(screen.getByRole("link", { name: "旧版回答依据" })).toBeVisible();
    expect(screen.getByRole("link", { name: "旧版 Atlas 助手" })).toBeVisible();
    expect(screen.queryByText(/票据|token|X-Nexus-Ticket/i)).not.toBeInTheDocument();
  });

  it("reports the unbound legacy workflow adapter as unavailable", async () => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      const url = String(input);
      if (url === "/api/session") return sessionResponse();
      if (url === "/api/legacy/workflows") {
        return new Response(JSON.stringify({ message: "unavailable" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(null, { status: 404 });
    });

    render(<ConsoleShell initialPath="/advanced/legacy/workflows" />);
    expect(await screen.findByText("请先开启高级维护模式")).toBeVisible();
    fireEvent.click(screen.getByRole("checkbox", { name: "高级维护模式" }));
    expect(await screen.findByText("这项旧版能力尚未接入安全会话，当前已停止操作。")).toBeVisible();
  });

  it("disables assistant attachment actions after the BFF reports capability unavailable", async () => {
    vi.mocked(fetch).mockImplementation(async (input) => {
      const url = String(input);
      if (url === "/api/session") return sessionResponse();
      if (url === "/api/legacy/assistant") {
        return new Response(JSON.stringify({ message: "unavailable" }), { status: 503, headers: { "Content-Type": "application/json" } });
      }
      return new Response(null, { status: 404 });
    });

    render(<ConsoleShell initialPath="/advanced/legacy/assistant" />);
    expect(await screen.findByText("请先开启高级维护模式")).toBeVisible();
    fireEvent.click(screen.getByRole("checkbox", { name: "高级维护模式" }));
    expect(await screen.findByText("这项旧版能力尚未接入安全会话，当前已停止操作。")).toBeVisible();
    const input = await screen.findByLabelText("选择要安全上传的附件");
    expect(input).toBeDisabled();
    expect(screen.getByRole("button", { name: "安全上传附件" })).toBeDisabled();
    expect(fetch).not.toHaveBeenCalledWith("/api/legacy/assistant/attachments", expect.anything());
  });
});

function withinNavigation(navigation: HTMLElement, label: string) {
  const link = Array.from(navigation.querySelectorAll("a")).find((candidate) => candidate.textContent === label);
  if (!link) throw new Error(`missing navigation link: ${label}`);
  return link;
}
