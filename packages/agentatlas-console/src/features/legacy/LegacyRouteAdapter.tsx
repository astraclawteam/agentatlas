import { useEffect, useState } from "react";
import { Button } from "@xiaozhiclaw/runtime-ui";
import { Paperclip } from "lucide-react";
import { Navigate } from "react-router-dom";

import { api, ApiError } from "../../api/client";
import { useSession } from "../../app/session";

export type LegacySurface = "knowledge" | "dream" | "workflows" | "evidence" | "assistant";

const titles: Record<LegacySurface, string> = {
  knowledge: "旧版知识维护",
  dream: "旧版梦境维护",
  workflows: "旧版流程维护",
  evidence: "旧版回答依据",
  assistant: "旧版 Atlas 助手",
};

interface LegacyItem {
  id: string;
  label: string;
  detail?: string;
}

export function LegacyRouteAdapter({ surface }: { surface: LegacySurface }) {
  const { session, advancedMode } = useSession();
  const [items, setItems] = useState<LegacyItem[]>([]);
  const [status, setStatus] = useState("正在读取已授权内容…");
  const [files, setFiles] = useState<File[]>([]);
  const [available, setAvailable] = useState(true);

  useEffect(() => {
    setItems([]);
    setFiles([]);
    setAvailable(true);
    if (!session.advanced_mode_allowed || !advancedMode) {
      setStatus("");
      return;
    }
    setStatus("正在读取已授权内容…");
    let active = true;
    api<{ items: LegacyItem[] }>(`/api/legacy/${surface}`)
      .then((result) => {
        if (!active) return;
        setItems(result.items);
        setStatus(result.items.length ? "" : "当前授权范围内暂无内容。");
      })
      .catch((error: unknown) => {
        if (!active) return;
        if (error instanceof ApiError && error.status === 503) setAvailable(false);
        setStatus(
          error instanceof ApiError && error.status === 503
            ? "这项旧版能力尚未接入安全会话，当前已停止操作。"
            : "无法读取这项旧版能力，请稍后重试。",
        );
      });
    return () => {
      active = false;
    };
  }, [advancedMode, session.advanced_mode_allowed, surface]);

  if (!session.advanced_mode_allowed) return <Navigate to="/knowledge" replace />;
  if (!advancedMode) {
    return (
      <section className="console-route-intro">
        <p className="console-route-kicker">高级维护 · 过渡入口</p>
        <h1 className="title-display">{titles[surface]}</h1>
        <p role="status">请先开启高级维护模式</p>
      </section>
    );
  }

  const upload = async () => {
    if (files.length === 0) return;
    const form = new FormData();
    files.forEach((file) => form.append("files", file, file.name));
    setStatus("正在通过安全会话上传…");
    try {
      await api<{ accepted: boolean }>("/api/legacy/assistant/attachments", {
        method: "POST",
        body: form,
      });
      setFiles([]);
      setStatus("附件已交给 Atlas 助手处理。");
    } catch (error) {
      if (error instanceof ApiError && error.status === 503) setAvailable(false);
      setStatus(
        error instanceof ApiError && error.status === 503
          ? "附件安全上传尚未接通，文件没有发送。"
          : "附件上传失败，文件没有发送。",
      );
    }
  };

  return (
    <section className="console-route-intro" aria-labelledby={`legacy-${surface}-title`}>
      <p className="console-route-kicker">高级维护 · 过渡入口</p>
      <h1 id={`legacy-${surface}-title`} className="title-display">
        {titles[surface]}
      </h1>
      <p>这里只显示当前企业会话和组织授权允许访问的旧版能力。</p>
      {surface === "assistant" ? (
        <div className="glass-rest console-legacy-upload">
          <label>
            <span>选择要安全上传的附件</span>
            <input
              aria-label="选择要安全上传的附件"
              type="file"
              multiple
              disabled={!available}
              onChange={(event) => setFiles(Array.from(event.currentTarget.files ?? []))}
            />
          </label>
          <Button disabled={!available || files.length === 0} onClick={() => void upload()}>
            <Paperclip aria-hidden size={18} />
            安全上传附件
          </Button>
        </div>
      ) : null}
      {status ? <p role="status">{status}</p> : null}
      {items.length ? (
        <ul className="console-legacy-items">
          {items.map((item) => (
            <li key={item.id} className="glass-rest">
              <strong>{item.label || "未命名内容"}</strong>
              {item.detail ? <span>{item.detail}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}
