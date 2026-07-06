import { render, screen, fireEvent } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AgentAtlasDashboard } from "./AgentAtlasDashboard";
import type { KnowledgeSpace, TimelineNode } from "./types";

const spaces: KnowledgeSpace[] = [
  { space_id: "spc_dept", enterprise_id: "e", kind: "department", name: "研发一部", org_scope: "department:d1", org_version: 1 },
  { space_id: "spc_emp", enterprise_id: "e", kind: "employee", name: "张予安", org_scope: "employee:u1", org_version: 1 },
];

const timeline: TimelineNode[] = [
  {
    timeline_node_id: "tl_1",
    space_id: "spc_emp",
    node_time: "2026-07-06T09:30:00+08:00",
    source_type: "work_brief",
    summary_text: "完成分拣规则联调",
  },
];

describe("AgentAtlasDashboard", () => {
  it("renders the four work surfaces and switches between them", () => {
    render(
      <AgentAtlasDashboard spaces={spaces} timeline={timeline} workflow={null} trace={null} />,
    );

    for (const label of ["组织知识地图", "梦境时间线", "知识工作流", "证据追溯"]) {
      expect(screen.getByRole("tab", { name: label })).toBeInTheDocument();
    }

    // knowledge map is the default surface
    expect(screen.getByTestId("knowledge-map")).toBeInTheDocument();
    expect(screen.getByText(/研发一部/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "梦境时间线" }));
    expect(screen.getByTestId("dream-timeline")).toBeInTheDocument();
    expect(screen.getByText("完成分拣规则联调")).toBeInTheDocument();
    expect(screen.getByText("工作简报")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "知识工作流" }));
    expect(screen.getByText(/暂无工作流/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "证据追溯" }));
    expect(screen.getByText(/暂无回答追溯记录/)).toBeInTheDocument();
  });

  it("shows the floating Atlas Agent launcher", () => {
    render(
      <AgentAtlasDashboard spaces={[]} timeline={[]} workflow={null} trace={null} />,
    );
    expect(screen.getByRole("button", { name: "打开 Atlas Agent" })).toBeInTheDocument();
  });
});
