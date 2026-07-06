import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { DreamPolicyPanel } from "./DreamPolicyPanel";

describe("DreamPolicyPanel", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("loads published policies with the global ticket and creates a new one", async () => {
    localStorage.setItem("atlas_console_ticket", "tick_admin");
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url: String(url), init });
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

    render(<DreamPolicyPanel />);
    await waitFor(() => expect(screen.getByText("project_group:pg_mes")).toBeInTheDocument());
    expect((calls[0].init?.headers as Record<string, string>)["X-Nexus-Ticket"]).toBe("tick_admin");

    fireEvent.change(screen.getByPlaceholderText("project_group:pg_mes"), { target: { value: "department:d1" } });
    fireEvent.change(screen.getByPlaceholderText(/组织知识地图/), { target: { value: "spc_dept" } });
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
    localStorage.setItem("atlas_console_ticket", "tick_bad");
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ message: "unauthorized" }), { status: 401 })),
    );
    render(<DreamPolicyPanel />);
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("加载策略失败"));
  });
});
