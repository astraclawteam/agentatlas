// 梦境策略面板：真连 atlas-agent 控制面（GET/POST /v1/dream-policies）。
// 票据取自头部的全局票据（api.getTicket）；控件复用 claw-runtime-ui 原语。
import { useCallback, useEffect, useState } from "react";
import { LegacyButton as ClawButton, LegacyInput as ClawInput } from "./app/runtime-ui-adapters";
import { createDreamPolicy, getTicket, listDreamPolicies } from "./api";

interface PublishedPolicy {
  dream_policy_id: string;
  org_scope: string;
  status: string;
  policy?: {
    schedule?: string;
    visibility_level?: string;
    output_space_id?: string;
    masking_rules?: string[];
    risk_signal_rules?: string[];
  };
}

const field: React.CSSProperties = { display: "flex", flexDirection: "column", gap: 4 };
const labelStyle: React.CSSProperties = { fontSize: 12, color: "var(--claw-text-secondary)", fontFamily: "var(--claw-font)" };

export function DreamPolicyPanel() {
  const [policies, setPolicies] = useState<PublishedPolicy[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    org_scope: "", schedule: "0 22 * * *", output_space_id: "",
    visibility_level: "members", evidence_retention: "pointer_plus_display_summary",
    masking_rules: "1[3-9]\\d{9}", risk_signal_rules: "风险[:：]\\S+",
  });

  const refresh = useCallback(async () => {
    if (!getTicket()) return;
    setError("");
    try {
      const body = await listDreamPolicies();
      setPolicies(body.dream_policies ?? []);
    } catch (e) {
      setError(`加载策略失败：${(e as Error).message}`);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await createDreamPolicy({
        org_scope: form.org_scope,
        schedule: form.schedule,
        input_sources: ["work_briefs"],
        visibility_level: form.visibility_level,
        masking_rules: form.masking_rules.split("\n").map((s) => s.trim()).filter(Boolean),
        risk_signal_rules: form.risk_signal_rules.split("\n").map((s) => s.trim()).filter(Boolean),
        evidence_retention: form.evidence_retention,
        output_space_id: form.output_space_id,
      });
      await refresh();
    } catch (err) {
      setError(`创建策略失败：${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ display: "flex", gap: 24, padding: 24, height: "100%", overflow: "auto", boxSizing: "border-box", fontFamily: "var(--claw-font)" }}>
      <form onSubmit={submit} style={{
        width: 340, flexShrink: 0, display: "flex", flexDirection: "column", gap: 12,
        padding: 16, borderRadius: "var(--claw-radius-md)", border: "1px solid var(--claw-border)",
        background: "var(--claw-surface-solid)", boxShadow: "var(--claw-shadow-1)", height: "fit-content",
      }}>
        <h2 style={{ margin: 0, fontSize: 15, color: "var(--claw-text)" }}>新建汇总策略</h2>
        <div style={{ fontSize: 12, color: "var(--claw-text-muted)" }}>
          也可以直接对右侧 Agent 说：“研发一部每晚汇总项目风险，不要暴露个人电话”。
        </div>
        <ClawInput label="汇总谁（组织范围）" required value={form.org_scope}
          onChange={(e) => setForm({ ...form, org_scope: e.target.value })} placeholder="project_group:pg_mes" />
        <ClawInput label="汇总结果放进哪个空间" required value={form.output_space_id}
          onChange={(e) => setForm({ ...form, output_space_id: e.target.value })} placeholder="spc_…（在“组织知识地图”里能看到）" />
        <ClawInput label="每天什么时候跑（cron，0 22 * * * = 每晚 22 点）" value={form.schedule}
          onChange={(e) => setForm({ ...form, schedule: e.target.value })} />
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-vis">谁能看到汇总</label>
          <select id="dp-vis" value={form.visibility_level}
            onChange={(e) => setForm({ ...form, visibility_level: e.target.value })}
            style={{
              padding: "7px 10px", fontSize: 13, borderRadius: "var(--claw-radius-sm)",
              border: "1px solid var(--claw-border-strong)", background: "var(--claw-surface-solid)",
              color: "var(--claw-text)", fontFamily: "var(--claw-font)",
            }}>
            <option value="members">成员可见</option>
            <option value="managers">管理者可见</option>
            <option value="company_sanitized">公司可见（默认脱敏）</option>
          </select>
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-mask">哪些内容要打码（正则，每行一条；默认打码手机号）</label>
          <textarea id="dp-mask" rows={2} value={form.masking_rules}
            onChange={(e) => setForm({ ...form, masking_rules: e.target.value })}
            style={{
              padding: "7px 10px", fontSize: 13, borderRadius: "var(--claw-radius-sm)",
              border: "1px solid var(--claw-border-strong)", background: "var(--claw-surface-solid)",
              color: "var(--claw-text)", fontFamily: "var(--claw-font)", resize: "vertical",
            }} />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-risk">什么算风险信号（正则，每行一条）</label>
          <textarea id="dp-risk" rows={2} value={form.risk_signal_rules}
            onChange={(e) => setForm({ ...form, risk_signal_rules: e.target.value })}
            style={{
              padding: "7px 10px", fontSize: 13, borderRadius: "var(--claw-radius-sm)",
              border: "1px solid var(--claw-border-strong)", background: "var(--claw-surface-solid)",
              color: "var(--claw-text)", fontFamily: "var(--claw-font)", resize: "vertical",
            }} />
        </div>
        <ClawButton type="submit" disabled={busy || !getTicket()}>
          {busy ? "创建中…" : "创建并发布策略"}
        </ClawButton>
        {error ? <div role="alert" style={{ fontSize: 12, color: "var(--claw-danger)" }}>{error}</div> : null}
        {!getTicket() ? <div style={{ fontSize: 12, color: "var(--claw-text-muted)" }}>先在右上角粘贴管理员票据。</div> : null}
      </form>

      <section style={{ flex: 1, minWidth: 0 }}>
        <h2 style={{ margin: "0 0 12px", fontSize: 15, color: "var(--claw-text)" }}>已发布策略（{policies.length}）</h2>
        {policies.length === 0 ? (
          <div style={{ color: "var(--claw-text-muted)", fontSize: 13 }}>
            还没有策略。左边建一条，或对右侧 Agent 说一句 —— 发布后每晚自动汇总。
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 10 }}>
            {policies.map((p) => (
              <li key={p.dream_policy_id} style={{
                padding: 14, borderRadius: "var(--claw-radius-md)", border: "1px solid var(--claw-border)",
                background: "var(--claw-surface-solid)", boxShadow: "var(--claw-shadow-1)",
              }}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
                  <strong style={{ fontSize: 13, color: "var(--claw-text)" }}>{p.org_scope}</strong>
                  <span style={{ fontSize: 12, color: "var(--claw-success)" }}>{p.status}</span>
                </div>
                <div style={{ marginTop: 6, fontSize: 12, color: "var(--claw-text-secondary)", display: "flex", gap: 14, flexWrap: "wrap" }}>
                  <span>调度 {p.policy?.schedule ?? "—"}</span>
                  <span>可见 {p.policy?.visibility_level ?? "—"}</span>
                  <span>输出空间 {p.policy?.output_space_id ?? "—"}</span>
                  <span>打码 {p.policy?.masking_rules?.length ?? 0} 条</span>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
