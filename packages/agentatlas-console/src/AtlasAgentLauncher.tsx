// Floating Atlas Agent entry. Sends messages to the agent control plane
// (POST /v1/agent/runs) with the AgentNexus ticket; errors surface in the
// conversation instead of being swallowed.
import { useCallback, useState } from "react";
import { AgentChatShell, type AgentChatMessage } from "@agentatlas/claw-runtime-ui";

interface AgentRunResponse {
  run_id: string;
  status: string;
  reply?: string;
  pending_confirmation?: { confirmation_id: string; summary: string };
}

async function callAgent(message: string, runID: string | null): Promise<AgentRunResponse> {
  const ticket = localStorage.getItem("atlas_ticket") ?? "";
  const url = runID ? `/v1/agent/runs/${runID}/messages` : "/v1/agent/runs";
  const body = runID ? { message } : { enterprise_id: localStorage.getItem("atlas_enterprise") ?? "", message };
  const resp = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Nexus-Ticket": ticket },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    throw new Error(`atlas-agent ${resp.status}: ${(await resp.text()).slice(0, 300)}`);
  }
  return (await resp.json()) as AgentRunResponse;
}

export function AtlasAgentLauncher() {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [runID, setRunID] = useState<string | null>(null);
  const [messages, setMessages] = useState<AgentChatMessage[]>([]);

  const onSend = useCallback(
    async (text: string) => {
      setMessages((prev) => [...prev, { id: `u_${Date.now()}`, role: "user", content: text }]);
      setBusy(true);
      try {
        const resp = await callAgent(text, runID);
        setRunID(resp.run_id);
        const content =
          resp.reply ??
          (resp.pending_confirmation
            ? `等待确认：${resp.pending_confirmation.summary}`
            : `运行状态：${resp.status}`);
        setMessages((prev) => [...prev, { id: `a_${Date.now()}`, role: "agent", content }]);
      } catch (err) {
        setMessages((prev) => [
          ...prev,
          { id: `e_${Date.now()}`, role: "agent", content: `请求失败：${(err as Error).message}` },
        ]);
      } finally {
        setBusy(false);
      }
    },
    [runID],
  );

  return (
    <>
      <button
        aria-label="打开 Atlas Agent"
        onClick={() => setOpen((v) => !v)}
        style={{
          position: "fixed",
          right: 24,
          bottom: 24,
          width: 52,
          height: 52,
          borderRadius: "50%",
          border: "none",
          cursor: "pointer",
          background: "var(--claw-accent)",
          color: "#fff",
          fontSize: 22,
          boxShadow: "var(--claw-shadow-3)",
          zIndex: 900,
        }}
      >
        ✦
      </button>
      {open ? (
        <div
          className="claw-glass"
          role="complementary"
          aria-label="Atlas Agent"
          style={{
            position: "fixed",
            right: 24,
            bottom: 88,
            width: 380,
            height: 480,
            display: "flex",
            flexDirection: "column",
            overflow: "hidden",
            zIndex: 900,
          }}
        >
          <div style={{ padding: "10px 14px", borderBottom: "1px solid var(--claw-border)", fontFamily: "var(--claw-font)", fontWeight: 600, color: "var(--claw-text)" }}>
            Atlas Agent
          </div>
          <AgentChatShell messages={messages} onSend={onSend} busy={busy} />
        </div>
      ) : null}
    </>
  );
}
