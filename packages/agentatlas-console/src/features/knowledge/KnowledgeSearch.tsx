import { useEffect, useRef } from "react";
import { Search, X } from "lucide-react";

interface KnowledgeSearchProps {
  expanded: boolean;
  value: string;
  onExpandedChange(expanded: boolean): void;
  onChange(value: string): void;
}

export function KnowledgeSearch({ expanded, value, onExpandedChange, onChange }: KnowledgeSearchProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (expanded) inputRef.current?.focus();
  }, [expanded]);

  if (!expanded) {
    return (
      <button type="button" className="knowledge-toolbar-action" aria-label="搜索已有内容" onClick={() => onExpandedChange(true)}>
        <Search aria-hidden size={18} strokeWidth={1.8} />
        搜索
      </button>
    );
  }
  return (
    <div className="knowledge-search" role="search">
      <Search aria-hidden size={18} strokeWidth={1.8} />
      <input
        ref={inputRef}
        type="search"
        aria-label="搜索已有内容"
        placeholder="输入名称或内容关键词"
        value={value}
        onChange={(event) => onChange(event.currentTarget.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            onChange("");
            onExpandedChange(false);
          }
        }}
      />
      <button type="button" aria-label="清除搜索" onClick={() => onChange("")}>
        <X aria-hidden size={17} />
      </button>
    </div>
  );
}
