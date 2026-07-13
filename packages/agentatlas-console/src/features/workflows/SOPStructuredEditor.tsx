import { ArrowDown, ArrowUp, CheckCircle2, GripVertical } from "lucide-react";

import type { AtlasWorkflow, AtlasWorkflowNode } from "../../types";
import { linearNodeOrder } from "./flowgram-adapter";

const LABELS: Record<string, string> = {
  "input.manual": "接收人工内容",
  "input.evidence_pointer": "读取已授权资料",
  "transform.extract_sop": "整理成标准步骤",
  "transform.summarize": "生成易读说明",
  "human.confirm": "负责人确认",
};

export function humanNodeLabel(node: AtlasWorkflowNode): string {
  return node.name?.trim() || LABELS[node.type] || "高级处理步骤";
}

export function reorderLinearWorkflow(workflow: AtlasWorkflow, from: number, to: number): AtlasWorkflow {
  const ordered = linearNodeOrder(workflow);
  if (!ordered || from === to || from < 0 || to < 0 || from >= ordered.length || to >= ordered.length) return workflow;
  const nodes = [...ordered];
  const [moved] = nodes.splice(from, 1);
  nodes.splice(to, 0, moved);
  return { ...workflow, nodes, edges: nodes.slice(0, -1).map((node, index) => ({ from: node.id, to: nodes[index + 1].id })) };
}

export function SOPStructuredEditor({ workflow, disabled, onChange }: { workflow: AtlasWorkflow; disabled?: boolean; onChange(workflow: AtlasWorkflow): void }) {
  const nodes = linearNodeOrder(workflow) ?? [];
  return (
    <section className="workflow-steps" aria-labelledby="workflow-steps-title">
      <div className="workflow-section-heading"><div><p>流程步骤</p><h2 id="workflow-steps-title">按执行顺序维护</h2></div><span>{nodes.length} 个步骤</span></div>
      <ol>
        {nodes.map((node, index) => (
          <li className="glass-rest" key={node.id}>
            <GripVertical aria-hidden size={18} />
            <span className="workflow-step-number">{index + 1}</span>
            <div><strong>{humanNodeLabel(node)}</strong><small>{node.requires_confirmation ? "执行到这里需要负责人确认" : "系统按顺序自动继续"}</small></div>
            {node.requires_confirmation ? <CheckCircle2 aria-label="需要确认" size={18} /> : null}
            <div className="workflow-step-actions">
              <button type="button" aria-label={`上移 ${humanNodeLabel(node)}`} disabled={disabled || index === 0} onClick={() => onChange(reorderLinearWorkflow(workflow, index, index - 1))}><ArrowUp aria-hidden size={17} /></button>
              <button type="button" aria-label={`下移 ${humanNodeLabel(node)}`} disabled={disabled || index === nodes.length - 1} onClick={() => onChange(reorderLinearWorkflow(workflow, index, index + 1))}><ArrowDown aria-hidden size={17} /></button>
            </div>
          </li>
        ))}
      </ol>
    </section>
  );
}
