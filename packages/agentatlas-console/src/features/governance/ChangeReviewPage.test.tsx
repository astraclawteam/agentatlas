import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ConsoleShell } from "../../app/ConsoleShell";
import type { Session } from "../../app/session";

const session: Session = {
  authenticated: true,
  enterprise_id: "ent-demo",
  enterprise_name: "示例企业",
  enterprise_user_id: "editor-user",
  display_name: "陈经理",
  org_version: 4,
  org_unit_ids: ["dept-rd"],
  org_tree: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }],
  permissions: ["suggest", "edit", "publish_low_risk", "approve_high_risk"],
};

const draft = {
  change_id: "change-1", enterprise_id: "ent-demo", org_unit_id: "dept-rd", resource_type: "knowledge_entry",
  resource_id: "knowledge-1", action: "update", requester_user_id: "editor-user", origin: "direct_edit",
  permission_mode: "direct_edit", revision: 2, state: "draft", base_version: 1,
  proposed_content: {
    title: "MES 异常工单处理", summary: "修正响应时间", sections: [{ heading: "处理方法", body: "十分钟内响应" }],
    references: ["设备维护手册"], impact: { people: 43, agent_answers: true, sops: ["异常处理 SOP"] },
  }, created_at: "2026-07-13T01:00:00Z", updated_at: "2026-07-13T01:02:00Z",
};

function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

function makeFetch(risk: "low" | "high", overrides: Record<string, (init?: RequestInit) => Response | Promise<Response>> = {}) {
  const calls: string[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    calls.push(`${init?.method ?? "GET"} ${url}`);
    if (url === "/api/session") return json(session);
    if (overrides[url]) return overrides[url](init);
    if (url === "/api/changes/change-1") return json({ draft, content: draft.proposed_content, base_content: { ...draft.proposed_content, summary: "原响应时间" } });
    if (url.endsWith("/diff")) return json({ before: { ...draft.proposed_content, summary: "原响应时间" }, after: draft.proposed_content });
    if (url.endsWith("/assess")) return json({ risk_level: risk, risk_reasons: risk === "low" ? ["bounded content change"] : ["changed approvals"] });
    if (url.endsWith("/submit")) return json(risk === "low"
      ? { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", risk_level: "low", mode: "single_confirmation", state: "pending", org_path: [] }
      : { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", reviewer_user_id: "manager-secret-id", risk_level: "high", mode: "upward_review", state: "pending", org_path: ["dept-rd", "company"] });
    if (url.endsWith("/decisions")) return new Response(null, { status: 204 });
    if (url.endsWith("/publish")) return json({ change_id: "change-1", resource_id: "knowledge-1", version: 2, audit_ref_id: "audit-hidden" });
    return json({ message: "not found" }, 404);
  });
  return { fn, calls };
}

describe("ChangeReviewPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("renders a nullable base and script-like text without crashing or creating markup", async () => {
    const script = '<script data-testid="injected">alert(1)</script>';
    const { fn } = makeFetch("low", {
      "/api/changes/change-1": () => json({ draft, content: { ...draft.proposed_content, title: script }, base_content: null }),
      "/api/changes/change-1/diff": () => json({ before: null, after: { ...draft.proposed_content, title: script } }),
    });
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    expect(await screen.findByText(script)).toBeVisible();
    expect(document.querySelector("script[data-testid='injected']")).toBeNull();
    expect(screen.getAllByText("未填写").length).toBeGreaterThan(0);
  });

  it("fails closed before rendering an oversized review payload", async () => {
    const unsafe = { ...draft.proposed_content, references: Array.from({ length: 201 }, (_, index) => `资料-${index}`) };
    const { fn } = makeFetch("low", {
      "/api/changes/change-1": () => json({ draft: { ...draft, proposed_content: unsafe }, content: unsafe, base_content: null }),
      "/api/changes/change-1/diff": () => json({ before: null, after: unsafe }),
    });
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    expect(await screen.findByText("内容过多或格式不安全")).toBeVisible();
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(0);
    expect(screen.queryByText("资料-200")).not.toBeInTheDocument();
  });

  it("shows every publishable section, reference, and SOP step field", async () => {
    const complete = {
      title: "完整 SOP",
      summary: "完整检查",
      sections: [{ heading: "第一部分", body: "第一段内容" }, { heading: "第二部分", body: "第二段内容" }],
      references: ["设备手册", "值班记录"],
      steps: [{ title: "检查工单", instruction: "打开工单", evidence: "工单截图", completion: "状态正常" }],
    };
    const { fn } = makeFetch("high", {
      "/api/changes/change-1": () => json({ draft: { ...draft, resource_type: "sop", proposed_content: complete }, content: complete, base_content: null }),
      "/api/changes/change-1/diff": () => json({ before: null, after: complete }),
    });
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    for (const text of ["第一部分", "第一段内容", "第二部分", "第二段内容", "设备手册", "值班记录", "检查工单", "打开工单", "工单截图", "状态正常"]) {
      expect(await screen.findByText(text)).toBeVisible();
    }
  });

  it("shows every impacted organization by human name without leaking unknown identifiers", async () => {
    const scopedSession: Session = {
      ...session,
      org_unit_ids: ["dept-rd", "dept-ops", "dept-unnamed"],
      org_tree: [
        { id: "dept-rd", name: "研发一部", selectable: true, children: [] },
        { id: "dept-ops", name: "生产中心", selectable: true, children: [] },
        { id: "dept-unnamed", name: "", selectable: true, children: [] },
      ],
    };
    const content = {
      ...draft.proposed_content,
      impact: { ...draft.proposed_content.impact, organizations: ["dept-rd", "dept-ops", "dept-unnamed", "secret-org-id"] },
    };
    const base = makeFetch("low", {
      "/api/changes/change-1": () => json({ draft: { ...draft, proposed_content: content }, content, base_content: null }),
      "/api/changes/change-1/diff": () => json({ before: null, after: content }),
    });
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => String(input) === "/api/session" ? Promise.resolve(json(scopedSession)) : base.fn(input, init)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    expect(await screen.findByText("生产中心")).toBeVisible();
    expect(screen.getByText("未命名组织")).toBeVisible();
    expect(screen.getByText("其他相关组织（1 个）")).toBeVisible();
    expect(screen.queryByText("secret-org-id")).not.toBeInTheDocument();
  });

  it("hands off low-risk work when the editor lacks publish permission", async () => {
    const limited = { ...session, permissions: ["edit"] };
    const route = { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", risk_level: "low", mode: "upward_review", state: "pending", org_path: ["dept-rd", "company"] };
    const base = makeFetch("low", { "/api/changes/change-1/submit": () => json(route) });
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => String(input) === "/api/session" ? Promise.resolve(json(limited)) : base.fn(input, init)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    fireEvent.click(await screen.findByRole("button", { name: "提交审核" }));
    expect(await screen.findByText("已提交给上级负责人复核")).toBeVisible();
    expect(screen.queryByRole("button", { name: "确认并发布" })).not.toBeInTheDocument();
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(0);
    expect(base.calls.filter((call) => call.endsWith("/submit"))).toHaveLength(1);
    expect(base.calls.some((call) => call.endsWith("/decisions"))).toBe(false);
    expect(base.calls.some((call) => call.endsWith("/publish"))).toBe(false);
  });

  it("reports an authoritative enterprise knowledge admin handoff truthfully", async () => {
    const limited = { ...session, permissions: ["edit"] };
    const route = { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", risk_level: "low", mode: "enterprise_knowledge_admin_queue", state: "pending", org_path: ["dept-rd"], queue: "enterprise-secret-queue" };
    const base = makeFetch("low", { "/api/changes/change-1/submit": () => json(route) });
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => String(input) === "/api/session" ? Promise.resolve(json(limited)) : base.fn(input, init)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    fireEvent.click(await screen.findByRole("button", { name: "提交审核" }));
    expect(await screen.findByText("已提交给企业知识管理员复核")).toBeVisible();
    expect(screen.queryByText(/enterprise-secret-queue/)).not.toBeInTheDocument();
    expect(base.calls.filter((call) => call.endsWith("/submit"))).toHaveLength(1);
    expect(base.calls.some((call) => call.endsWith("/decisions"))).toBe(false);
    expect(base.calls.some((call) => call.endsWith("/publish"))).toBe(false);
  });

  it("does not offer high-risk submission after exact resource edit permission is lost", async () => {
    const limited = { ...session, permissions: ["publish_low_risk"] };
    const base = makeFetch("high");
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => String(input) === "/api/session" ? Promise.resolve(json(limited)) : base.fn(input, init)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    expect(await screen.findByText("当前账号没有提交这项内容的权限")).toBeVisible();
    expect(screen.queryByRole("button", { name: "提交审核" })).not.toBeInTheDocument();
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(0);
  });

  it.each([
    ["another editor", { ...session, enterprise_user_id: "another-user", permissions: ["edit"] }, "knowledge_entry", "等待原发起人完成发布"],
    ["workflow non-editor", { ...session, permissions: ["edit"] }, "workflow", "当前账号没有发布这项内容的权限"],
  ])("does not offer approved publication to %s", async (_case, currentSession, resourceType, waitingCopy) => {
    const approved = { ...draft, state: "approved", resource_type: resourceType };
    const base = makeFetch("high", { "/api/changes/change-1": () => json({ draft: approved, content: draft.proposed_content, base_content: null }) });
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => String(input) === "/api/session" ? Promise.resolve(json(currentSession)) : base.fn(input, init)));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    expect(await screen.findByText(waitingCopy)).toBeVisible();
    expect(screen.queryByRole("button", { name: "发布已审核修改" })).not.toBeInTheDocument();
  });

  it.each([
    ["low", "确认并发布"],
    ["high", "提交审核"],
  ] as const)("shows exactly one primary action for %s risk", async (risk, label) => {
    const { fn } = makeFetch(risk);
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);

    expect(await screen.findByRole("button", { name: label })).toBeVisible();
    expect(document.querySelectorAll(".knowledge-primary-button")).toHaveLength(1);
    expect(screen.getByRole("heading", { name: "检查修改并确认下一步" })).toBeVisible();
    expect(screen.getByText("修改前")).toBeVisible();
    expect(screen.getByText("修改后")).toBeVisible();
    expect(screen.getByText(risk === "low" ? "内容范围有限" : "修改了审批规则")).toBeVisible();
    if (risk === "high") expect(screen.getByText("审批路径：提交后由系统确定复核负责人")).toBeVisible();
    expect(screen.getByText("研发一部")).toBeVisible();
    expect(screen.getByText("43 人")).toBeVisible();
    expect(screen.getByText("员工 Agent 的相关回答会更新")).toBeVisible();
    expect(screen.getByText("异常处理 SOP")).toBeVisible();
    expect(screen.queryByText(/manager-secret-id|audit-hidden|bounded content change|changed approvals/)).not.toBeInTheDocument();
  });

  it("uses one stable key when a low-risk publish response is retried after remount", async () => {
    const publishKeys: string[] = [];
    let publishAttempt = 0;
    const { fn } = makeFetch("low", {
      "/api/changes/change-1": () => json({
        draft: publishAttempt ? { ...draft, state: "approved" } : draft,
        content: draft.proposed_content,
        base_content: { ...draft.proposed_content, summary: "原响应时间" },
      }),
      "/api/changes/change-1/publish": (init) => {
        publishKeys.push(new Headers(init?.headers).get("Idempotency-Key") ?? "");
        publishAttempt += 1;
        return publishAttempt === 1 ? json({ message: "结果暂时未知" }, 503) : json({ change_id: "change-1", resource_id: "knowledge-1", version: 2, audit_ref_id: "hidden" });
      },
    });
    vi.stubGlobal("fetch", fn);
    const firstPage = render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "确认并发布" }));

    expect(await screen.findByRole("button", { name: "重试发布" })).toBeVisible();
    firstPage.unmount();
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "发布已审核修改" }));
    expect(await screen.findByText("修改已发布")).toBeVisible();
    expect(publishKeys).toHaveLength(2);
    expect(publishKeys[0]).toBe(publishKeys[1]);
    expect(publishKeys[0].length).toBeGreaterThanOrEqual(16);
  });

  it("retries a low-risk confirmation with the same key after response loss and remount", async () => {
    const decisionKeys: string[] = [];
    let decisionAttempt = 0;
    const submittedRoute = { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", risk_level: "low", mode: "single_confirmation", state: "pending", org_path: [] };
    const { fn } = makeFetch("low", {
      "/api/changes/change-1": () => json({ draft: decisionAttempt ? { ...draft, state: "submitted" } : draft, content: draft.proposed_content, base_content: {} , route: decisionAttempt ? submittedRoute : undefined }),
      "/api/changes/change-1/decisions": (init) => {
        decisionKeys.push(new Headers(init?.headers).get("Idempotency-Key") ?? "");
        decisionAttempt += 1;
        return decisionAttempt === 1 ? json({ message: "结果暂时未知" }, 503) : new Response(null, { status: 204 });
      },
    });
    vi.stubGlobal("fetch", fn);
    const firstPage = render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "确认并发布" }));
    expect(await screen.findByText(/这一步暂时没有完成/)).toBeVisible();
    firstPage.unmount();

    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "继续确认并发布" }));
    expect(await screen.findByText("修改已发布")).toBeVisible();
    expect(decisionKeys).toHaveLength(2);
    expect(decisionKeys[0]).toBe(decisionKeys[1]);
  });

  it("recovers a committed low-risk submit after its response is lost", async () => {
    let reads = 0;
    const calls: string[] = [];
    const submittedRoute = { change_id: "change-1", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor-user", risk_level: "low", mode: "single_confirmation", state: "pending", org_path: [] };
    const base = makeFetch("low", {
      "/api/changes/change-1": () => {
        reads += 1;
        return json({ draft: reads === 1 ? draft : { ...draft, state: "submitted" }, content: draft.proposed_content, base_content: null, route: reads === 1 ? undefined : submittedRoute });
      },
      "/api/changes/change-1/submit": () => json({ message: "响应丢失" }, 503),
    });
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(`${init?.method ?? "GET"} ${String(input)}`);
      return base.fn(input, init);
    }));
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "确认并发布" }));
    expect(await screen.findByText("修改已发布")).toBeVisible();
    expect(reads).toBe(2);
    expect(calls.filter((call) => call.endsWith("/decisions"))).toHaveLength(1);
    expect(calls.filter((call) => call.endsWith("/publish"))).toHaveLength(1);
    expect(screen.queryByText(/这一步暂时没有完成/)).not.toBeInTheDocument();
  });

  it("submits high risk upward without exposing reviewer IDs or allowing self-review", async () => {
    const { fn, calls } = makeFetch("high");
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/changes/change-1/review" />);
    fireEvent.click(await screen.findByRole("button", { name: "提交审核" }));

    expect(await screen.findByText("已提交给上级负责人复核")).toBeVisible();
    expect(screen.getByText("审批路径：研发一部 → 上级组织")).toBeVisible();
    expect(screen.queryByRole("button", { name: /批准|通过|复核通过/ })).not.toBeInTheDocument();
    expect(screen.queryByText("manager-secret-id")).not.toBeInTheDocument();
    expect(calls.some((call) => call.endsWith("/decisions"))).toBe(false);
  });

  it("loads the scoped review shortcut and lists only actionable human-readable changes", async () => {
    const { fn } = makeFetch("high", {
      "/api/changes?org_unit_id=dept-rd&limit=100": () => json({ items: [{ draft: { ...draft, state: "submitted", requester_user_id: "another-user" }, content: draft.proposed_content, route: { mode: "upward_review", reviewer_user_id: "editor-user", state: "pending" } }] }),
    });
    vi.stubGlobal("fetch", fn);
    render(<ConsoleShell initialPath="/knowledge/dept-rd/reviews" />);

    expect(await screen.findByRole("heading", { name: "处理建议与审核" })).toBeVisible();
    expect(screen.getByText("MES 异常工单处理")).toBeVisible();
    expect(screen.getByRole("link", { name: "检查修改" })).toHaveAttribute("href", "/knowledge/dept-rd/changes/change-1/review");
    expect(screen.queryByText(/change-1|another-user|editor-user/)).not.toBeInTheDocument();
  });
});
