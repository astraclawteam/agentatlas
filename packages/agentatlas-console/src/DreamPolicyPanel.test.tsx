import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { DreamPolicyPanel } from "./DreamPolicyPanel";

describe("DreamPolicyPanel", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("loads published policies with the pasted ticket and creates a new one", async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url, init });
        if ((init?.method ?? "GET") === "GET") {
          return new Response(
            JSON.stringify({
              dream_policies: [
                {
                  dream_policy_id: "dp_1",
                  org_scope: "project_group:pg_mes",
                  status: "published",
                  policy: { schedule: "0 22 * * *", visibility_level: "members", output_space_id: "spc_pg", masking_rules: ["a"] },
                },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response(JSON.stringify({ dream_policy_id: "dp_2", version: 1 }), { status: 201 });
      }),
    );

    render(<DreamPolicyPanel agentBaseUrl="http://agent.test" />);

    // paste the admin ticket -> list loads
    fireEvent.change(screen.getByLabelText(/管理员票据/), { target: { value: "tick_admin" } });
    await waitFor(() => expect(screen.getByText("project_group:pg_mes")).toBeInTheDocument());
    expect(calls[0].url).toBe("http://agent.test/v1/dream-policies");
    expect((calls[0].init?.headers as Record<string, string>)["X-Nexus-Ticket"]).toBe("tick_admin");

    // create a policy -> POST body carries the form + rules split by line
    fireEvent.change(screen.getByLabelText(/org_scope/), { target: { value: "department:d1" } });
    fireEvent.change(screen.getByLabelText(/output_space_id/), { target: { value: "spc_dept" } });
    fireEvent.click(screen.getByRole("button", { name: /创建并发布策略/ }));
    await waitFor(() => {
      const post = calls.find((c) => c.init?.method === "POST");
      expect(post).toBeTruthy();
      const body = JSON.parse(String(post!.init!.body));
      expect(body.org_scope).toBe("department:d1");
      expect(body.output_space_id).toBe("spc_dept");
      expect(body.masking_rules).toEqual(["1[3-9]\\d{9}"]);
    });
  });

  it("surfaces backend failures loudly", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ message: "unauthorized" }), { status: 401 })),
    );
    render(<DreamPolicyPanel agentBaseUrl="http://agent.test" />);
    fireEvent.change(screen.getByLabelText(/管理员票据/), { target: { value: "tick_bad" } });
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("加载策略失败"));
  });
});
