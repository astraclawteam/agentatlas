// Atlas Agent 常驻对话面板 —— 管理界面的主入口（产品设计 §4/§14：
// 管理员用自然语言 + 附件与 AgentAtlas 交互，而不是填写大量表单）。
// 附件先经 /v1/artifacts/jobs 入库解析，再把编号清单告知 Agent。
import { useCallback, useState } from "react";
import { AgentChatShell, numberAttachments, type AgentChatMessage, type Attachment } from "@agentatlas/claw-runtime-ui";
import { agentMessage, agentRun, getTicket, uploadArtifact } from "./api";

const HINTS = [
  "把刚上传的文档整理成研发一部的 SOP 流程",
  "研发一部最近有哪些风险？",
  "解释最近一条回答用了哪些证据",
];

export function AtlasAgentPanel() {
  const [busy, setBusy] = useState(false);
  const [runID, setRunID] = useState<string | null>(null);
  const [messages, setMessages] = useState<AgentChatMessage[]>([]);

  const push = (role: AgentChatMessage["role"], content: string) =>
    setMessages((prev) => [...prev, { id: `${role}_${Date.now()}_${prev.length}`, role, content }]);

  const onSend = useCallback(
    async (text: string, attachments: Attachment[]) => {
      if (!getTicket()) {
        push("agent", "请先在右上角粘贴管理员票据（X-Nexus-Ticket），再与我对话。");
        return;
      }
      const labels = numberAttachments(attachments);
      const shown = text || `（上传 ${attachments.map((_, i) => labels[i]).join("、")}）`;
      push("user", shown);
      setBusy(true);
      try {
        // 1) 附件全部入库：上传即触发解析与索引（知识进入库）。
        const uploaded: string[] = [];
        for (let i = 0; i < attachments.length; i++) {
          const att = attachments[i];
          const res = await uploadArtifact(att.file);
          uploaded.push(`${labels[i]}《${att.filename}》(artifact_id=${res.artifact_id})`);
        }
        if (uploaded.length > 0) {
          push("agent", `已入库并开始解析：${uploaded.length} 份资料。解析完成后可检索、可生成 SOP。`);
        }
        // 2) 有话要说才驱动 Agent；纯上传则到此为止。
        const finalText =
          uploaded.length > 0
            ? `${text || "请确认这些资料入库整理。"}\n（本轮已上传：${uploaded.join("；")}）`
            : text;
        if (text || uploaded.length === 0) {
          const resp = runID ? await agentMessage(runID, finalText) : await agentRun(finalText);
          setRunID(resp.run_id);
          const toolNote =
            resp.tool_calls && resp.tool_calls.length > 0
              ? `\n（本轮调用工具：${resp.tool_calls.map((t) => t.name).join("、")}）`
              : "";
          push("agent", (resp.text || "（无回复文本）") + toolNote);
        }
      } catch (err) {
        push("agent", `请求失败：${(err as Error).message}`);
      } finally {
        setBusy(false);
      }
    },
    [runID],
  );

  return (
    <aside
      aria-label="Atlas Agent"
      style={{
        width: 380,
        flexShrink: 0,
        display: "flex",
        flexDirection: "column",
        borderLeft: "1px solid var(--claw-border)",
        background: "var(--claw-surface)",
        backdropFilter: "blur(var(--claw-surface-blur))",
      }}
    >
      <div style={{ padding: "12px 16px", borderBottom: "1px solid var(--claw-border)", fontFamily: "var(--claw-font)" }}>
        <div style={{ fontWeight: 600, color: "var(--claw-text)" }}>Atlas Agent</div>
        <div style={{ marginTop: 2, fontSize: 12, color: "var(--claw-text-muted)" }}>
          用一句话导入资料、生成 SOP、查风险、解释回答 —— 附件点 📎
        </div>
      </div>
      {messages.length === 0 ? (
        <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 8 }}>
          <div style={{ fontSize: 12, color: "var(--claw-text-muted)", fontFamily: "var(--claw-font)" }}>可以这样开始：</div>
          {HINTS.map((h) => (
            <button
              key={h}
              type="button"
              onClick={() => void onSend(h, [])}
              style={{
                textAlign: "left", padding: "8px 12px", fontSize: 13, cursor: "pointer",
                fontFamily: "var(--claw-font)", color: "var(--claw-text-secondary)",
                background: "var(--claw-surface-solid)", border: "1px solid var(--claw-border)",
                borderRadius: "var(--claw-radius-sm)",
              }}
            >
              “{h}”
            </button>
          ))}
        </div>
      ) : null}
      <div style={{ flex: 1, minHeight: 0 }}>
        <AgentChatShell messages={messages} onSend={onSend} busy={busy} />
      </div>
    </aside>
  );
}
