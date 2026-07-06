// Console entry. Until the runtime answer API (Goal 12) is wired, the
// dashboard renders a clearly-labeled sample dataset so every surface is
// visible; the Atlas Agent launcher already talks to the real control plane.
import { AgentAtlasDashboard } from "./AgentAtlasDashboard";
import type { AnswerTraceView, AtlasWorkflow, KnowledgeSpace, TimelineNode } from "./types";

const sampleSpaces: KnowledgeSpace[] = [
  { space_id: "spc_company", enterprise_id: "ent_demo", kind: "company", name: "顺视智能制造集团", org_scope: "company:c1", org_version: 4 },
  { space_id: "spc_bu", enterprise_id: "ent_demo", kind: "business_unit", name: "智能制造事业部", org_scope: "business_unit:bu1", org_version: 4 },
  { space_id: "spc_dept", enterprise_id: "ent_demo", kind: "department", name: "研发一部", org_scope: "department:d1", org_version: 4 },
  { space_id: "spc_pg", enterprise_id: "ent_demo", kind: "project_group", name: "MES 异常工单专项组", org_scope: "project_group:pg1", org_version: 4 },
  { space_id: "spc_emp", enterprise_id: "ent_demo", kind: "employee", name: "张予安", org_scope: "employee:u1", org_version: 4 },
];

const sampleLinks = [
  { from: "spc_company", to: "spc_bu" },
  { from: "spc_bu", to: "spc_dept" },
  { from: "spc_dept", to: "spc_pg" },
  { from: "spc_pg", to: "spc_emp" },
];

const sampleTimeline: TimelineNode[] = [
  {
    timeline_node_id: "tl_1",
    space_id: "spc_emp",
    node_time: "2026-07-06T09:30:00+08:00",
    source_type: "work_brief",
    summary_text: "完成 MES 分拣规则联调；风险：接口限流方案未定。",
    tags: ["MES", "联调"],
    evidence_pointer_id: "ev_brief_1",
  },
  {
    timeline_node_id: "tl_2",
    space_id: "spc_pg",
    node_time: "2026-07-05T22:00:00+08:00",
    source_type: "dream_summary",
    summary_text: "项目组梦境：本周工单处理延迟下降 18%，遗留风险 1 项。",
    evidence_pointer_id: "ev_dream_1",
  },
];

const sampleWorkflow: AtlasWorkflow = {
  workflow_id: "wf_mes_sop_demo",
  version: 0,
  kind: "sop",
  nodes: [
    { id: "in", type: "input.evidence_pointer", name: "接收 SOP 文档" },
    { id: "parse", type: "parser.document", name: "解析文档" },
    { id: "extract", type: "transform.extract_sop", name: "抽取 SOP 结构" },
    { id: "confirm", type: "human.confirm", name: "管理员确认", requires_confirmation: true },
    { id: "trace", type: "trace.append", name: "写入追溯" },
  ],
  edges: [
    { from: "in", to: "parse" },
    { from: "parse", to: "extract" },
    { from: "extract", to: "confirm" },
    { from: "confirm", to: "trace", condition: "approved" },
  ],
  risk_level: "medium",
};

const sampleTrace: AnswerTraceView = {
  trace_id: "tr_demo",
  sanitized_question_summary: "员工询问当前工作内容",
  space_ids: ["spc_emp", "spc_pg"],
  evidence_pointer_ids: ["ev_brief_1"],
  agentnexus_read_grant_ids: ["grant_1"],
  model_route: "llmrouter/deepseek-v4",
  answer_hash: "sha256:2f7a9c31d0be",
};

export default function App() {
  return (
    <AgentAtlasDashboard
      spaces={sampleSpaces}
      spaceLinks={sampleLinks}
      timeline={sampleTimeline}
      workflow={sampleWorkflow}
      trace={sampleTrace}
      banner="示例数据集（后端 answer/spaces API 于 Goal 12 接入后替换为真实数据；Atlas Agent 按钮已直连 /v1/agent/runs）"
    />
  );
}
