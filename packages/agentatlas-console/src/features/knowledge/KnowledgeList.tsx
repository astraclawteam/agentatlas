import { ChevronRight, FileText } from "lucide-react";

import type { KnowledgeItem } from "../../api/knowledge";

interface KnowledgeListProps {
  items: KnowledgeItem[];
  loading: boolean;
  error: boolean;
  organizationName: string;
  query: string;
  onRetry(): void;
}

export function KnowledgeList({ items, loading, error, organizationName, query, onRetry }: KnowledgeListProps) {
  if (loading) {
    return <div className="knowledge-state" aria-busy="true" aria-live="polite">正在读取{organizationName}的知识…</div>;
  }
  if (error) {
    return (
      <div className="knowledge-state" role="alert">
        <strong>暂时无法读取企业知识</strong>
        <span>你的内容没有丢失，请检查网络后重试。</span>
        <button type="button" className="knowledge-secondary-button" onClick={onRetry}>重新读取</button>
      </div>
    );
  }
  if (items.length === 0) {
    return (
      <div className="knowledge-state">
        <strong>{query ? "没有找到相符的内容" : "这个范围还没有可用内容"}</strong>
        <span>{query ? "换一个更短的关键词，或清除搜索查看全部内容。" : "先添加一份资料，系统会帮助你整理成可用知识。"}</span>
      </div>
    );
  }
  return (
    <ul className="knowledge-list">
      {items.map((item) => (
        <li key={item.key} className="glass-rest knowledge-row">
          <span className="knowledge-row-icon"><FileText aria-hidden size={19} strokeWidth={1.8} /></span>
          <span className="knowledge-row-copy">
            <strong>{item.title}</strong>
            <small>{item.type_label} · {item.updated_label} · {item.scope_label}</small>
          </span>
          <a href={`/knowledge/item/${encodeURIComponent(item.key)}`} aria-label={`查看 ${item.title}`} className="knowledge-row-link">
            查看 <ChevronRight aria-hidden size={16} />
          </a>
        </li>
      ))}
    </ul>
  );
}
