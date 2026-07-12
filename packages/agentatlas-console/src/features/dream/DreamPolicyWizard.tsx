import { useState } from "react";
import type { BasicDreamPolicy, DreamSource } from "../../api/dream";

const sourceChoices: Array<[DreamSource, string]> = [["work_brief", "工作简报"], ["child_dream_summary", "下级组织记忆"], ["project_record", "项目记录"], ["sop_update", "SOP 变更"], ["completed_task", "已完成任务"], ["risk_event", "风险事件"]];

export function DreamPolicyWizard({ organizations, onSubmit, advancedAllowed = false, advancedMode = false }: { organizations: Array<{ id: string; name: string }>; onSubmit?: (value: BasicDreamPolicy) => void | Promise<void>; advancedAllowed?: boolean; advancedMode?: boolean }) {
  const [value, setValue] = useState<BasicDreamPolicy>({ organization: organizations[0]?.id ?? "", cadence: "nightly", inputSources: ["work_brief", "child_dream_summary"], visibility: "managers", confirmation: "high_risk_only" });
  return <form className="dream-wizard" onSubmit={(event) => { event.preventDefault(); void onSubmit?.(value); }}>
    <div className="dream-wizard-grid">
      <WizardCard title="整理哪个组织"><label className="dream-field">组织<select value={value.organization} onChange={(event) => setValue({ ...value, organization: event.target.value })}>{organizations.map((org) => <option key={org.id} value={org.id}>{org.name}</option>)}</select></label></WizardCard>
      <WizardCard title="多久整理一次"><Choices name="cadence" values={[["nightly", "每晚"], ["weekly", "每周"]]} selected={value.cadence} onChange={(cadence) => setValue({ ...value, cadence: cadence as BasicDreamPolicy["cadence"] })} /></WizardCard>
      <WizardCard title="使用哪些记录"><div className="dream-choice-list">{sourceChoices.map(([source, label]) => <label className="dream-choice" key={source}><input type="checkbox" checked={value.inputSources.includes(source)} onChange={(event) => setValue({ ...value, inputSources: event.target.checked ? [...value.inputSources, source] : value.inputSources.filter((item) => item !== source) })} />{label}</label>)}</div></WizardCard>
      <WizardCard title="谁能看到"><Choices name="visibility" values={[["members", "组织成员"], ["managers", "组织负责人"], ["company_sanitized", "全公司（已脱敏）"]]} selected={value.visibility} onChange={(visibility) => setValue({ ...value, visibility: visibility as BasicDreamPolicy["visibility"] })} /></WizardCard>
      <WizardCard title="是否需要确认"><Choices name="confirmation" values={[["always", "每次都确认"], ["high_risk_only", "发现风险时确认"], ["never", "自动完成"]]} selected={value.confirmation} onChange={(confirmation) => setValue({ ...value, confirmation: confirmation as BasicDreamPolicy["confirmation"] })} /></WizardCard>
    </div>
    {advancedAllowed && advancedMode ? <section className="dream-advanced"><h2>高级运行设置</h2><p>固定版本的梦境工作流</p><div className="dream-wizard-grid"><label className="dream-field">时区<input aria-label="时区" defaultValue="Asia/Shanghai" /></label><label className="dream-field">运行表达式<input aria-label="运行表达式" defaultValue="0 22 * * *" /></label><label className="dream-field">脱敏规则<textarea aria-label="脱敏规则" /></label><label className="dream-field">运行诊断<textarea aria-label="运行诊断" readOnly value="等待下一次运行" /></label></div><div className="glass-rest dream-state"><strong>高级梦境工作流</strong><span>固定版本的工作流将在高级工作流编辑器中打开；发布版本保持只读。</span></div></section> : null}
    <div className="dream-actions"><button className="dream-primary" type="submit" disabled={!value.organization || value.inputSources.length === 0}>保存梦境工作流</button></div>
  </form>;
}
function WizardCard({ title, children }: { title: string; children: React.ReactNode }) { return <section className="glass-rest dream-wizard-card"><h2>{title}</h2>{children}</section>; }
function Choices({ name, values, selected, onChange }: { name: string; values: Array<[string, string]>; selected: string; onChange: (value: string) => void }) { return <div className="dream-choice-list">{values.map(([value, label]) => <label className="dream-choice" key={value}><input type="radio" name={name} value={value} checked={selected === value} onChange={() => onChange(value)} />{label}</label>)}</div>; }
