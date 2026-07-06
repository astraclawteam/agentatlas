// Attachment primitives mirrored from xiaozhiclaw-runtime ui-next
// (components/chat/attachment-panel.tsx): same object model, same per-type
// numbering (图片1/视频1…), token-styled for the console (no tailwind here).
export interface Attachment {
  id: string;
  type: "image" | "audio" | "video" | "file";
  filename: string;
  mimeType?: string;
  size?: number;
  /** Object URL for local preview */
  previewUrl?: string;
  /** Raw File reference for upload */
  file: File;
}

const TYPE_ICONS: Record<Attachment["type"], string> = {
  image: "🖼️",
  audio: "🎵",
  video: "🎬",
  file: "📄",
};

const TYPE_LABELS: Record<Attachment["type"], string> = {
  image: "图片",
  audio: "音频",
  video: "视频",
  file: "文件",
};

/** Derive attachment type from MIME (mirrors runtime inferAttachmentType). */
export function inferAttachmentType(mimeType: string): Attachment["type"] {
  if (mimeType.startsWith("image/")) return "image";
  if (mimeType.startsWith("audio/")) return "audio";
  if (mimeType.startsWith("video/")) return "video";
  return "file";
}

/** Per-type, in-order numbering — 图片1/图片2/视频1 …（与发送给 Agent 的顺序一致）. */
export function numberAttachments(attachments: Attachment[]): string[] {
  const counters: Partial<Record<Attachment["type"], number>> = {};
  return attachments.map((att) => {
    const n = (counters[att.type] = (counters[att.type] ?? 0) + 1);
    return `${TYPE_LABELS[att.type]}${n}`;
  });
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)}KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
}

export interface AttachmentStripProps {
  attachments: Attachment[];
  onRemove: (id: string) => void;
}

/** Horizontal strip of pending attachments, rendered above the chat input. */
export function AttachmentStrip({ attachments, onRemove }: AttachmentStripProps) {
  if (attachments.length === 0) return null;
  const labels = numberAttachments(attachments);
  return (
    <div style={{ display: "flex", flexWrap: "wrap", gap: 6, padding: "6px 10px 0" }}>
      {attachments.map((att, i) => (
        <div
          key={att.id}
          style={{
            display: "flex", alignItems: "center", gap: 6, maxWidth: 220,
            padding: "4px 8px", fontSize: 12, fontFamily: "var(--claw-font)",
            border: "1px solid var(--claw-border)", borderRadius: "var(--claw-radius-sm)",
            background: "var(--claw-surface-solid)",
          }}
        >
          <span style={{
            padding: "0 6px", fontSize: 10, fontWeight: 600, borderRadius: 999,
            background: "var(--claw-accent-soft)", color: "var(--claw-accent)",
          }}>
            {labels[i]}
          </span>
          {att.type === "image" && att.previewUrl ? (
            <img src={att.previewUrl} alt={att.filename} style={{ width: 24, height: 24, borderRadius: 4, objectFit: "cover" }} />
          ) : (
            <span aria-hidden>{TYPE_ICONS[att.type]}</span>
          )}
          <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", color: "var(--claw-text-secondary)" }}>
            {att.filename}
          </span>
          {att.size != null ? (
            <span style={{ color: "var(--claw-text-muted)", flexShrink: 0 }}>{formatSize(att.size)}</span>
          ) : null}
          <button
            type="button"
            aria-label={`移除${labels[i]}`}
            onClick={() => onRemove(att.id)}
            style={{
              border: "none", background: "transparent", cursor: "pointer",
              color: "var(--claw-text-muted)", fontSize: 12, padding: 0, lineHeight: 1,
            }}
          >
            ✕
          </button>
        </div>
      ))}
    </div>
  );
}
