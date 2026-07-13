import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AtlasWorkflowCanvas, toWorkflowJSON } from "./AtlasWorkflowCanvas";
import type { AtlasWorkflow } from "./types";

const workflow: AtlasWorkflow = {
  workflow_id: "wf_mes_sop",
  version: 1,
  kind: "sop",
  nodes: [
    { id: "in", type: "input.evidence_pointer", name: "接收 SOP 文档" },
    { id: "extract", type: "transform.extract_sop", name: "抽取 SOP 结构" },
    { id: "confirm", type: "human.confirm", name: "管理员确认", requires_confirmation: true },
  ],
  edges: [
    { from: "in", to: "extract" },
    { from: "extract", to: "confirm" },
  ],
  risk_level: "medium",
};

describe("toWorkflowJSON", () => {
  it("maps nodes/edges into FlowGram document JSON with layered positions", () => {
    const json = toWorkflowJSON(workflow);
    expect(json.nodes).toHaveLength(3);
    expect(json.edges).toMatchObject([
      { sourceNodeID: "in", targetNodeID: "extract" },
      { sourceNodeID: "extract", targetNodeID: "confirm" },
    ]);
    const xs = json.nodes.map((n) => n.meta?.position?.x);
    expect(new Set(xs).size).toBe(3); // three BFS layers -> three columns
    expect(json.nodes[0]?.data).toMatchObject({ title: "接收 SOP 文档", atlasType: "input.evidence_pointer" });
  });
});

describe("AtlasWorkflowCanvas", () => {
  it("renders the summary bar and the FlowGram canvas host", () => {
    render(<AtlasWorkflowCanvas workflow={workflow} readonly />);
    expect(screen.getByText("wf_mes_sop")).toBeInTheDocument();
    expect(screen.getByTestId("canvas-node-count")).toHaveTextContent("节点 3");
    expect(screen.getByText("确认点 1")).toBeInTheDocument();
    expect(screen.getByText("风险 medium")).toBeInTheDocument();
    expect(screen.getByTestId("atlas-workflow-canvas")).toBeInTheDocument();
  });
});
