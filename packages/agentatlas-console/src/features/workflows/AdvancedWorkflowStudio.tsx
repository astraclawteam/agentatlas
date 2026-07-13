import type { AtlasWorkflow } from "../../types";
import { AtlasWorkflowCanvas } from "../../AtlasWorkflowCanvas";
import { fromWorkflowJSON } from "./flowgram-adapter";

export function AdvancedWorkflowStudio({ draft, onChange, readonly }: { draft: AtlasWorkflow; onChange(workflow: AtlasWorkflow): void; readonly?: boolean }) {
  return (
    <section className="advanced-workflow-studio" data-testid="advanced-workflow-studio" aria-labelledby="advanced-workflow-title">
      <header><p>高级维护模式</p><h2 id="advanced-workflow-title">流程画布与条件设置</h2><span>画布导出后仍保存到当前草稿，不会直接发布。</span></header>
      <AtlasWorkflowCanvas workflow={draft} readonly={readonly} onExport={(json) => onChange(fromWorkflowJSON(json, draft))} />
    </section>
  );
}
