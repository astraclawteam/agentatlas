import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";

import type { SOPStep } from "../../api/changes";

export function SOPStepsEditor({ steps, onChange }: { steps: SOPStep[]; onChange(steps: SOPStep[]): void }) {
  const update = (index: number, patch: Partial<SOPStep>) => onChange(steps.map((step, candidate) => candidate === index ? { ...step, ...patch } : step));
  const move = (index: number, direction: -1 | 1) => {
    const target = index + direction;
    if (target < 0 || target >= steps.length) return;
    const next = [...steps];
    [next[index], next[target]] = [next[target], next[index]];
    onChange(next);
  };
  return (
    <section aria-labelledby="sop-steps-title">
      <div className="knowledge-editor-section-heading">
        <div><p className="knowledge-eyebrow">执行说明</p><h2 id="sop-steps-title">处理步骤</h2></div>
        <button className="knowledge-secondary-button" type="button" onClick={() => onChange([...steps, { title: "", instruction: "" }])}>
          <Plus aria-hidden size={17} /> 添加一步
        </button>
      </div>
      <ol className="sop-step-list">
        {steps.map((step, index) => (
          <li className="glass-rest sop-step-card" key={index}>
            <span className="sop-step-number" aria-hidden>{index + 1}</span>
            <div className="sop-step-fields">
              <label>步骤名称<input aria-label={`第 ${index + 1} 步名称`} value={step.title} onChange={(event) => update(index, { title: event.currentTarget.value })} /></label>
              <label>怎么做<textarea aria-label={`第 ${index + 1} 步怎么做`} value={step.instruction} onChange={(event) => update(index, { instruction: event.currentTarget.value })} /></label>
              <label>需要留下什么依据<input aria-label={`第 ${index + 1} 步依据`} value={step.evidence ?? ""} onChange={(event) => update(index, { evidence: event.currentTarget.value })} /></label>
              <label>怎样算完成<input aria-label={`第 ${index + 1} 步完成标准`} value={step.completion ?? ""} onChange={(event) => update(index, { completion: event.currentTarget.value })} /></label>
            </div>
            <div className="sop-step-actions" aria-label={`调整第 ${index + 1} 步`}>
              <button type="button" aria-label={`上移第 ${index + 1} 步`} disabled={index === 0} onClick={() => move(index, -1)}><ArrowUp aria-hidden size={17} /></button>
              <button type="button" aria-label={`下移第 ${index + 1} 步`} disabled={index === steps.length - 1} onClick={() => move(index, 1)}><ArrowDown aria-hidden size={17} /></button>
              <button type="button" aria-label={`删除第 ${index + 1} 步`} disabled={steps.length === 1} onClick={() => onChange(steps.filter((_, candidate) => candidate !== index))}><Trash2 aria-hidden size={17} /></button>
            </div>
          </li>
        ))}
      </ol>
    </section>
  );
}

