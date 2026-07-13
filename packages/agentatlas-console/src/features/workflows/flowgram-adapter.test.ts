import { describe, expect, it } from "vitest";

import type { AtlasWorkflow } from "../../types";
import { fromWorkflowJSON, isBasicLinearWorkflow, toWorkflowJSON } from "./flowgram-adapter";

function advancedWorkflowFixture(): AtlasWorkflow {
  return {
    workflow_id: "wf-risk-review",
    version: 7,
    kind: "sop",
    risk_level: "high",
    variables: { reviewer: { type: "string", default: "manager" } },
    input_schema: { type: "object", required: ["request"] },
    output_schema: { type: "object", properties: { approved: { type: "boolean" } } },
    nodes: [
      { id: "start", type: "input.manual", name: "接收申请", config: { channel: "agent" } },
      { id: "judge", type: "custom.risk_gate", name: "判断风险", config: { threshold: 80 }, requires_confirmation: true },
      { id: "confirm", type: "human.confirm", name: "负责人确认", requires_confirmation: true },
    ],
    edges: [
      { from: "start", to: "judge" },
      { from: "judge", to: "confirm", condition: "risk == 'high'" },
    ],
  };
}

describe("FlowGram adapter", () => {
  it("round-trips every canonical Atlas field, including unknown node types", () => {
    const original = advancedWorkflowFixture();
    const restored = fromWorkflowJSON(toWorkflowJSON(original), original);

    expect(restored).toEqual(original);
    expect(restored.edges[1].condition).toBe("risk == 'high'");
  });

  const supported = (): AtlasWorkflow => ({
    workflow_id: "wf-basic", version: 0, kind: "sop", risk_level: "low",
    nodes: [
      { id: "a", type: "input.manual", name: "开始" },
      { id: "b", type: "transform.extract_sop", name: "整理" },
      { id: "c", type: "human.confirm", name: "确认" },
    ],
    edges: [{ from: "a", to: "b" }, { from: "b", to: "c" }],
  });

  it("accepts only a connected, acyclic, supported linear SOP", () => {
    expect(isBasicLinearWorkflow(supported())).toBe(true);
  });

  it.each([
    ["branch", (workflow: AtlasWorkflow) => { workflow.edges = [{ from: "a", to: "b" }, { from: "a", to: "c" }]; }],
    ["conditional edge", (workflow: AtlasWorkflow) => { workflow.edges[0].condition = "approved"; }],
    ["unknown node", (workflow: AtlasWorkflow) => { workflow.nodes[1].type = "custom.future"; }],
    ["cycle", (workflow: AtlasWorkflow) => { workflow.edges = [{ from: "a", to: "b" }, { from: "b", to: "a" }, { from: "b", to: "c" }]; }],
    ["disconnected node", (workflow: AtlasWorkflow) => { workflow.edges = [{ from: "a", to: "b" }]; }],
    ["duplicate edge", (workflow: AtlasWorkflow) => { workflow.edges = [{ from: "a", to: "b" }, { from: "a", to: "b" }]; }],
    ["multiple incoming edges", (workflow: AtlasWorkflow) => { workflow.edges = [{ from: "a", to: "c" }, { from: "b", to: "c" }]; }],
  ])("rejects %s instead of inferring a step order", (_name, mutate) => {
    const workflow = supported();
    mutate(workflow);
    expect(isBasicLinearWorkflow(workflow)).toBe(false);
  });

  it("reconstructs user-added and deleted FlowGram nodes and edges without reviving removed data", () => {
    const original = supported();
    const json = toWorkflowJSON(original);
    json.nodes = json.nodes.filter((node) => node.id !== "b");
    json.nodes.push({ id: "d", type: "human.confirm", data: { title: "主管确认", atlasType: "human.confirm", requiresConfirmation: true }, meta: { position: { x: 500, y: 80 } } });
    json.edges = [{ sourceNodeID: "a", targetNodeID: "d" }];

    const restored = fromWorkflowJSON(json, original);
    expect(restored.nodes.map((node) => node.id)).toEqual(["a", "c", "d"]);
    expect(restored.nodes[2]).toMatchObject({ id: "d", type: "human.confirm", name: "主管确认", requires_confirmation: true });
    expect(restored.edges).toEqual([{ from: "a", to: "d" }]);
  });
});
