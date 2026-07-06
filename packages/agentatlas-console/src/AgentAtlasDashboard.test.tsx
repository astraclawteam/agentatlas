import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AgentAtlasDashboard } from "./AgentAtlasDashboard";

function stubBackend() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const u = String(url);
      if (u.endsWith("/v1/spaces")) {
        return new Response(
          JSON.stringify({
            spaces: [
              { space_id: "spc_pg", enterprise_id: "e1", kind: "project_group", name: "MES 异常工单专项组", org_scope: "project_group:pg1", org_version: 1 },
              { space_id: "spc_emp", enterprise_id: "e1", kind: "employee", name: "张予安", org_scope: "employee:u1", org_version: 1 },
            ],
          }),
          { status: 200 },
        );
      }
      if (u.includes("/timeline")) {
        return new Response(
          JSON.stringify({
            nodes: [{
              timeline_node_id: "tl1", space_id: "spc_emp", node_time: "2026-07-06T09:00:00+08:00",
              source_type: "work_brief", summary_text: "完成分拣规则联调。", tags: [], evidence_pointer_id: "ev1",
            }],
          }),
          { status: 200 },
        );
      }
      if (u.endsWith("/v1/traces")) {
        return new Response(
          JSON.stringify({
            traces: [{
              trace_id: "tr1", sanitized_question_summary: "员工询问工作内容",
              space_ids: ["spc_emp"], evidence_pointer_ids: ["ev1"],
              agentnexus_read_grant_ids: ["g1"], model_route: "llmrouter/x", answer_hash: "sha256:aa",
            }],
          }),
          { status: 200 },
        );
      }
      if (u.endsWith("/v1/dream-policies")) {
        return new Response(JSON.stringify({ dream_policies: [] }), { status: 200 });
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
}

describe("AgentAtlasDashboard", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("guides the first-time user until a ticket is pasted, then loads real data", async () => {
    stubBackend();
    render(<AgentAtlasDashboard />);
    expect(screen.getByText(/欢迎使用 AgentAtlas/)).toBeInTheDocument();
    expect(screen.getByText(/粘贴管理员票据/)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("管理员票据"), { target: { value: "tick_admin" } });
    await waitFor(() => {
      const hits = screen.getAllByText((content) => content.includes("MES 异常工单专项组"));
      expect(hits.length).toBeGreaterThan(0);
    });
  });

  it("every surface carries a plain-language description and the agent panel is always present", async () => {
    stubBackend();
    localStorage.setItem("atlas_console_ticket", "tick_admin");
    render(<AgentAtlasDashboard />);
    expect(screen.getByRole("complementary", { name: "Atlas Agent" })).toBeInTheDocument();
    expect(screen.getByText(/随组织架构自动生成/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "证据追溯" }));
    expect(screen.getByText(/出处清单/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("员工询问工作内容")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "知识工作流" }));
    expect(screen.getByText("添加步骤")).toBeInTheDocument();
    expect(screen.getByText("人工确认")).toBeInTheDocument();
  });
});
