import { afterEach, describe, expect, it, vi } from "vitest";

import { diffChange, getChange, normalizeContent } from "./changes";

const expectedLimits = { sections: 100, steps: 200, references: 200, impactItems: 200, titleLength: 500, bodyLength: 20_000, people: 10_000_000 };

function json(value: unknown) {
  return new Response(JSON.stringify(value), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("governed change JSON boundary", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("normalizes nullable and malformed nested content without throwing", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input) => String(input).endsWith("/diff")
      ? json({ before: null, after: { title: 42, summary: {}, sections: {}, steps: "bad", references: { bad: true }, impact: { people: "many", agent_answers: "yes", sops: {}, organizations: 42 } } })
      : json({ draft: { change_id: "change-1", org_unit_id: "dept-rd", resource_type: "knowledge_entry", resource_id: "knowledge-1", requester_user_id: "editor", permission_mode: "direct_edit", revision: 1, state: "draft", proposed_content: {}, updated_at: "2026-07-13T00:00:00Z" }, content: { sections: {}, references: {} }, base_content: null })));

    await expect(getChange("change-1")).resolves.toMatchObject({ content: { title: "", summary: "", sections: [], references: [] }, base_content: { title: "", summary: "" } });
    const diff = await diffChange("change-1");
    expect(diff).toEqual({
      before: expect.objectContaining({ title: "", summary: "", sections: [], steps: [], references: [] }),
      after: expect.objectContaining({ title: "", summary: "", sections: [], steps: [], references: [] }),
    });
    expect(diff.after.impact).not.toHaveProperty("people");
    expect(diff.after.impact).not.toHaveProperty("agent_answers");
  });

  it("preserves script-like strings as inert text while normalizing every nested field", () => {
    const script = '<script>alert("x")</script>';
    const normalized = normalizeContent({
      title: script,
      summary: script,
      sections: [{ heading: script, body: script }],
      steps: [{ title: script, instruction: script, evidence: script, completion: script }],
      references: [script, 42],
      impact: { people: 3, agent_answers: true, sops: [script, null], organizations: [script] },
    });
    expect(normalized).toMatchObject({
      title: script,
      summary: script,
      sections: [{ heading: script, body: script }],
      steps: [{ title: script, instruction: script, evidence: script, completion: script }],
      references: [script],
      impact: { people: 3, agent_answers: true, sops: [script], organizations: [script] },
    });
  });

  it("fails closed instead of truncating oversized known content", () => {
    const section = { heading: "标题", body: "正文" };
    expect(() => normalizeContent({ sections: Array.from({ length: expectedLimits.sections + 1 }, () => section) })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent({ steps: Array.from({ length: expectedLimits.steps + 1 }, () => ({ title: "步骤", instruction: "说明" })) })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent({ references: Array.from({ length: expectedLimits.references + 1 }, () => "资料") })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent({ impact: { organizations: Array.from({ length: expectedLimits.impactItems + 1 }, () => "org") } })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent({ title: "字".repeat(expectedLimits.titleLength + 1) })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent({ sections: [{ heading: "标题", body: "字".repeat(expectedLimits.bodyLength + 1) }] })).toThrow("内容过多或格式不安全");
    expect(() => normalizeContent(JSON.stringify({ references: Array.from({ length: expectedLimits.references + 1 }, () => "资料") }))).toThrow("内容过多或格式不安全");
  });

  it.each([1.5, -1, expectedLimits.people + 1, Number.MAX_SAFE_INTEGER])("rejects unsafe impacted people count %s", (people) => {
    expect(() => normalizeContent({ impact: { people } })).toThrow("内容过多或格式不安全");
  });
});
