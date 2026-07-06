// 梦境策略面板：真连 atlas-agent 控制面（GET/POST /v1/dream-policies）。
// 票据（X-Nexus-Ticket）由使用者粘贴，存 localStorage —— 与生产一致的单一鉴权路径。
import { useCallback, useEffect, useState } from "react";

const AGENT_BASE = (import.meta as any).env?.VITE_ATLAS_AGENT_URL ?? "http://localhost:8081";
const TICKET_KEY = "atlas_console_ticket";

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
    evidence_retention?: string;
  };
}

const field: React.CSSProperties = { display: "flex", flexDirection: "column", gap: 4 };
const labelStyle: React.CSSProperties = { fontSize: 12, color: "var(--claw-text-secondary)" };
const inputStyle: React.CSSProperties = {
  padding: "7px 10px", fontSize: 13, borderRadius: "var(--claw-radius-sm)",
  border: "1px solid var(--claw-border-strong)", background: "var(--claw-surface-solid)",
  color: "var(--claw-text)", fontFamily: "var(--claw-font)",
};

export function DreamPolicyPanel({ agentBaseUrl = AGENT_BASE }: { agentBaseUrl?: string }) {
  const [ticket, setTicket] = useState(() => localStorage.getItem(TICKET_KEY) ?? "");
  const [policies, setPolicies] = useState<PublishedPolicy[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    org_scope: "", schedule: "0 22 * * *", output_space_id: "",
    visibility_level: "members", evidence_retention: "pointer_plus_display_summary",
    masking_rules: "1[3-9]\\d{9}", risk_signal_rules: "风险[:：]\\S+",
  });

  const authedFetch = useCallback(
    (path: string, init?: RequestInit) =>
      fetch(agentBaseUrl + path, {
        ...init,
        headers: { "Content-Type": "application/json", "X-Nexus-Ticket": ticket, ...(init?.headers ?? {}) },
      }),
    [agentBaseUrl, ticket],
  );

  const refresh = useCallback(async () => {
    if (!ticket) return;
    setError("");
    try {
      const resp = await authedFetch("/v1/dream-policies");
      const body = await resp.json();
      if (!resp.ok) throw new Error(body.message ?? `HTTP ${resp.status}`);
      setPolicies(body.dream_policies ?? []);
    } catch (e) {
      setError(`加载策略失败：${(e as Error).message}`);
    }
  }, [authedFetch, ticket]);

  useEffect(() => {
    localStorage.setItem(TICKET_KEY, ticket);
    void refresh();
  }, [ticket, refresh]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const resp = await authedFetch("/v1/dream-policies", {
        method: "POST",
        body: JSON.stringify({
          org_scope: form.org_scope,
          schedule: form.schedule,
          input_sources: ["work_briefs"],
          visibility_level: form.visibility_level,
          masking_rules: form.masking_rules.split("\n").map((s) => s.trim()).filter(Boolean),
          risk_signal_rules: form.risk_signal_rules.split("\n").map((s) => s.trim()).filter(Boolean),
          evidence_retention: form.evidence_retention,
          output_space_id: form.output_space_id,
        }),
      });
      const body = await resp.json();
      if (!resp.ok) throw new Error(body.message ?? `HTTP ${resp.status}`);
      await refresh();
    } catch (err) {
      setError(`创建策略失败：${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ display: "flex", gap: 24, padding: 24, height: "100%", overflow: "auto", boxSizing: "border-box" }}>
      <form onSubmit={submit} style={{
        width: 360, flexShrink: 0, display: "flex", flexDirection: "column", gap: 12,
        padding: 16, borderRadius: "var(--claw-radius-md)", border: "1px solid var(--claw-border)",
        background: "var(--claw-surface-solid)", boxShadow: "var(--claw-shadow-1)", height: "fit-content",
      }}>
        <h2 style={{ margin: 0, fontSize: 15, color: "var(--claw-text)" }}>新建梦境策略</h2>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-ticket">管理员票据（X-Nexus-Ticket）</label>
          <input id="dp-ticket" style={inputStyle} value={ticket} onChange={(e) => setTicket(e.target.value)} placeholder="tick_..." />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-scope">组织范围 org_scope</label>
          <input id="dp-scope" style={inputStyle} required value={form.org_scope}
            onChange={(e) => setForm({ ...form, org_scope: e.target.value })} placeholder="project_group:pg_mes" />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-space">输出空间 output_space_id</label>
          <input id="dp-space" style={inputStyle} required value={form.output_space_id}
            onChange={(e) => setForm({ ...form, output_space_id: e.target.value })} placeholder="spc_..." />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-cron">调度（cron，每晚 22 点 = 0 22 * * *）</label>
          <input id="dp-cron" style={inputStyle} value={form.schedule}
            onChange={(e) => setForm({ ...form, schedule: e.target.value })} />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-vis">可见范围</label>
          <select id="dp-vis" style={inputStyle} value={form.visibility_level}
            onChange={(e) => setForm({ ...form, visibility_level: e.target.value })}>
            <option value="members">成员可见</option>
            <option value="managers">管理者可见</option>
            <option value="company">公司可见（默认脱敏）</option>
          </select>
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-mask">脱敏规则（正则，每行一条）</label>
          <textarea id="dp-mask" rows={2} style={inputStyle} value={form.masking_rules}
            onChange={(e) => setForm({ ...form, masking_rules: e.target.value })} />
        </div>
        <div style={field}>
          <label style={labelStyle} htmlFor="dp-risk">风险信号规则（正则，每行一条）</label>
          <textarea id="dp-risk" rows={2} style={inputStyle} value={form.risk_signal_rules}
            onChange={(e) => setForm({ ...form, risk_signal_rules: e.target.value })} />
        </div>
        <button type="submit" disabled={busy || !ticket} style={{
          padding: "9px 14px", fontSize: 13, fontWeight: 600, cursor: busy ? "wait" : "pointer",
          border: "none", borderRadius: "var(--claw-radius-sm)",
          background: "var(--claw-accent)", color: "#fff", opacity: busy || !ticket ? 0.6 : 1,
        }}>
          {busy ? "创建中…" : "创建并发布策略"}
        </button>
        {error ? <div role="alert" style={{ fontSize: 12, color: "var(--claw-danger)" }}>{error}</div> : null}
        {!ticket ? <div style={{ fontSize: 12, color: "var(--claw-text-muted)" }}>粘贴管理员票据后可加载与创建策略。</div> : null}
      </form>

      <section style={{ flex: 1, minWidth: 0 }}>
        <h2 style={{ margin: "0 0 12px", fontSize: 15, color: "var(--claw-text)" }}>已发布策略（{policies.length}）</h2>
        {policies.length === 0 ? (
          <div style={{ color: "var(--claw-text-muted)", fontSize: 13 }}>暂无已发布策略。</div>
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
                  <span>脱敏 {p.policy?.masking_rules?.length ?? 0} 条</span>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
