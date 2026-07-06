import { useState } from "react";
import type { KeyboardEvent } from "react";
import { ClawButton } from "./button";

export interface AgentChatMessage {
  id: string;
  role: "user" | "agent";
  content: string;
}

export interface AgentChatShellProps {
  messages: AgentChatMessage[];
  onSend: (text: string) => void;
  busy?: boolean;
  placeholder?: string;
}

/** Chat input shell in the AstraClaw agent-conversation style. */
export function AgentChatShell({ messages, onSend, busy, placeholder }: AgentChatShellProps) {
  const [draft, setDraft] = useState("");

  const send = () => {
    const text = draft.trim();
    if (!text || busy) return;
    onSend(text);
    setDraft("");
  };
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%", fontFamily: "var(--claw-font)" }}>
      <div role="log" aria-label="对话" style={{ flex: 1, overflowY: "auto", display: "flex", flexDirection: "column", gap: 8, padding: 12 }}>
        {messages.map((m) => (
          <div
            key={m.id}
            style={{
              alignSelf: m.role === "user" ? "flex-end" : "flex-start",
              maxWidth: "82%",
              padding: "8px 12px",
              fontSize: 14,
              whiteSpace: "pre-wrap",
              color: m.role === "user" ? "#fff" : "var(--claw-text)",
              background: m.role === "user" ? "var(--claw-accent)" : "var(--claw-surface-solid)",
              border: m.role === "user" ? "none" : "1px solid var(--claw-border)",
              borderRadius: "var(--claw-radius-md)",
              boxShadow: "var(--claw-shadow-1)",
            }}
          >
            {m.content}
          </div>
        ))}
        {busy ? <div style={{ fontSize: 12, color: "var(--claw-text-muted)" }}>Atlas Agent 正在思考…</div> : null}
      </div>
      <div style={{ display: "flex", gap: 8, padding: 12, borderTop: "1px solid var(--claw-border)" }}>
        <textarea
          aria-label="消息输入"
          value={draft}
          placeholder={placeholder ?? "向 Atlas Agent 描述你要做的事…"}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKeyDown}
          rows={2}
          style={{
            flex: 1,
            resize: "none",
            padding: "8px 12px",
            fontSize: 14,
            fontFamily: "var(--claw-font)",
            color: "var(--claw-text)",
            background: "var(--claw-surface-solid)",
            border: "1px solid var(--claw-border-strong)",
            borderRadius: "var(--claw-radius-sm)",
            outline: "none",
          }}
        />
        <ClawButton onClick={send} disabled={busy || draft.trim() === ""}>
          发送
        </ClawButton>
      </div>
    </div>
  );
}
