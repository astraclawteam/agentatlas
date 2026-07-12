// Atlas Agent 常驻对话面板 —— 管理界面的主入口（产品设计 §4/§14：
// 管理员用自然语言 + 附件与 AgentAtlas 交互，而不是填写大量表单）。
// 附件先经 /v1/artifacts/jobs 入库解析，再把编号清单告知 Agent。
import { useCallback, useRef, useState } from "react";
import {
  AgentChatShell,
  Button,
  type AgentChatShellLabels,
  type RuntimeAttachment,
  type RuntimeMessage,
} from "@xiaozhiclaw/runtime-ui";
import { Paperclip } from "lucide-react";
import { agentMessage, agentRun, getTicket, uploadArtifact } from "./api";

const HINTS = [
  "把刚上传的文档整理成研发一部的 SOP 流程",
  "研发一部最近有哪些风险？",
  "解释最近一条回答用了哪些证据",
];

const CHAT_LABELS: AgentChatShellLabels = {
  conversation: "Atlas 助手对话",
  promptInput: { send: "发送" },
  attachments: {
    uploading: "正在上传",
    uploadFailed: "上传失败",
    remove: (name) => `移除 ${name}`,
  },
  messageList: {
    list: "对话消息",
    roles: { user: "你", assistant: "Atlas 助手", system: "系统" },
  },
};

export function AtlasAgentPanel() {
  const [busy, setBusy] = useState(false);
  const [runID, setRunID] = useState<string | null>(null);
  const [messages, setMessages] = useState<RuntimeMessage[]>([]);
  const [value, setValue] = useState("");
  const [attachments, setAttachments] = useState<Array<{ view: RuntimeAttachment; file: File }>>([]);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const push = (role: RuntimeMessage["role"], content: string) =>
    setMessages((prev) => [...prev, { id: `${role}_${Date.now()}_${prev.length}`, role, content }]);

  const onSend = useCallback(
    async ({ text, attachments: submitted }: { text: string; attachments: RuntimeAttachment[] }) => {
      if (!getTicket()) {
        push("assistant", "当前会话尚未获得维护权限。");
        return;
      }
      if (!text.trim() && submitted.length === 0) return;
      push("user", text || `上传 ${submitted.length} 份资料`);
      setValue("");
      setBusy(true);
      try {
        const uploaded: string[] = [];
        const failed: string[] = [];
        for (const [index, attachment] of submitted.entries()) {
          const pending = attachments.find((candidate) => candidate.view.id === attachment.id);
          if (!pending) continue;
          try {
            const result = await uploadArtifact(pending.file);
            uploaded.push(`资料${index + 1}《${attachment.name}》(artifact_id=${result.artifact_id})`);
          } catch (error) {
            failed.push(`资料${index + 1}《${attachment.name}》：${(error as Error).message}`);
          }
        }
        setAttachments([]);
        if (uploaded.length > 0) {
          push("assistant", `已入库并开始解析：${uploaded.length} 份资料。`);
        }
        if (failed.length > 0) {
          push("assistant", `以下 ${failed.length} 份上传失败，请重试：\n${failed.join("\n")}`);
        }
        const finalText = uploaded.length
          ? `${text || "请确认这些资料入库整理。"}\n（本轮已上传：${uploaded.join("；")}）`
          : text;
        if (!finalText.trim()) return;
        const resp = runID ? await agentMessage(runID, finalText) : await agentRun(finalText);
        setRunID(resp.run_id);
        const toolNote =
          resp.tool_calls && resp.tool_calls.length > 0
            ? `\n（本轮调用工具：${resp.tool_calls.map((t) => t.name).join("、")}）`
            : "";
        push("assistant", (resp.text || "（无回复文本）") + toolNote);
      } catch (err) {
        push("assistant", `请求失败：${(err as Error).message}`);
      } finally {
        setBusy(false);
      }
    },
    [attachments, runID],
  );

  const stageFiles = (files: File[]) => {
    setAttachments((current) => [
      ...current,
      ...files.map((file, index) => ({
        file,
        view: {
          id: `attachment-${Date.now()}-${index}`,
          name: file.name,
          size: file.size,
          state: "ready" as const,
        },
      })),
    ]);
  };

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
        WebkitBackdropFilter: "blur(var(--claw-surface-blur))",
      }}
    >
      <div style={{ padding: "12px 16px", borderBottom: "1px solid var(--claw-border)", fontFamily: "var(--claw-font)" }}>
        <div style={{ fontWeight: 600, color: "var(--claw-text)" }}>Atlas Agent</div>
        <div style={{ marginTop: 2, fontSize: 12, color: "var(--claw-text-muted)" }}>
          用一句话生成 SOP、查风险或解释回答
        </div>
      </div>
      {messages.length === 0 ? (
        <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 8 }}>
          <div style={{ fontSize: 12, color: "var(--claw-text-muted)", fontFamily: "var(--claw-font)" }}>可以这样开始：</div>
          {HINTS.map((h) => (
            <button
              key={h}
              type="button"
              className="claw-tap-card"
              onClick={() => void onSend({ text: h, attachments: [] })}
              style={{ padding: "8px 12px", fontSize: 13, color: "var(--claw-text-secondary)" }}
            >
              “{h}”
            </button>
          ))}
        </div>
      ) : null}
      <div style={{ flex: 1, minHeight: 0 }}>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          hidden
          aria-label="选择附件"
          onChange={(event) => {
            stageFiles(Array.from(event.currentTarget.files ?? []));
            event.currentTarget.value = "";
          }}
        />
        <Button aria-label="添加附件" onClick={() => fileInputRef.current?.click()} disabled={busy}>
          <Paperclip aria-hidden size={18} />
        </Button>
        <AgentChatShell
          labels={CHAT_LABELS}
          messages={messages}
          value={value}
          attachments={attachments.map((attachment) => attachment.view)}
          onChange={setValue}
          onSend={onSend}
          onRemoveAttachment={(id) =>
            setAttachments((current) => current.filter((attachment) => attachment.view.id !== id))
          }
          onPasteFiles={stageFiles}
          onDropFiles={stageFiles}
          busy={busy}
        />
      </div>
    </aside>
  );
}
