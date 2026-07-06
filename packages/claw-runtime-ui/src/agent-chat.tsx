import { useRef, useState } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import { ClawButton } from "./button";
import { AttachmentStrip, inferAttachmentType, type Attachment } from "./attachment";

export interface AgentChatMessage {
  id: string;
  role: "user" | "agent";
  content: string;
}

export interface AgentChatShellProps {
  messages: AgentChatMessage[];
  /** attachments follow the runtime composer contract: staged files ride along
   *  with the message and are cleared after send. */
  onSend: (text: string, attachments: Attachment[]) => void;
  busy?: boolean;
  placeholder?: string;
  /** false hides the attach button (e.g. read-only embeds). Default true. */
  allowAttachments?: boolean;
}

/** Chat input shell in the AstraClaw agent-conversation style, with the
 *  runtime's attachment strip (图片1/视频1… numbering). */
export function AgentChatShell({ messages, onSend, busy, placeholder, allowAttachments = true }: AgentChatShellProps) {
  const [draft, setDraft] = useState("");
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const send = () => {
    const text = draft.trim();
    if ((text === "" && attachments.length === 0) || busy) return;
    onSend(text, attachments);
    setDraft("");
    setAttachments([]);
  };
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  };
  const onPickFiles = (e: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files ?? []);
    if (files.length === 0) return;
    setAttachments((prev) => [
      ...prev,
      ...files.map((file, i) => {
        const type = inferAttachmentType(file.type || "");
        return {
          id: `att_${Date.now()}_${i}`,
          type,
          filename: file.name,
          mimeType: file.type || undefined,
          size: file.size,
          previewUrl: type === "image" || type === "audio" || type === "video" ? URL.createObjectURL(file) : undefined,
          file,
        } satisfies Attachment;
      }),
    ]);
    e.target.value = ""; // allow re-picking the same file
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
      <AttachmentStrip attachments={attachments} onRemove={(id) => setAttachments((prev) => prev.filter((a) => a.id !== id))} />
      <div style={{ display: "flex", gap: 8, padding: 12, borderTop: "1px solid var(--claw-border)", alignItems: "flex-end" }}>
        {allowAttachments ? (
          <>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              hidden
              aria-label="选择附件"
              onChange={onPickFiles}
              accept="application/pdf,image/*,audio/*,video/*,.md,.txt,.docx,.pptx,.xlsx"
            />
            <ClawButton
              variant="ghost"
              aria-label="添加附件"
              title="添加文档/图片/音频/视频"
              onClick={() => fileInputRef.current?.click()}
              disabled={busy}
            >
              📎
            </ClawButton>
          </>
        ) : null}
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
        <ClawButton onClick={send} disabled={busy || (draft.trim() === "" && attachments.length === 0)}>
          发送
        </ClawButton>
      </div>
    </div>
  );
}
