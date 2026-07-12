import { DockedPanel } from "@xiaozhiclaw/runtime-ui";
import { BookOpenCheck, LibraryBig, MoonStar, Workflow } from "lucide-react";
import { Navigate, Route, Routes, useNavigate, useParams } from "react-router-dom";

import { LegacyRouteAdapter, type LegacySurface } from "../features/legacy/LegacyRouteAdapter";
import { KnowledgeHome } from "../features/knowledge/KnowledgeHome";
import { KnowledgeTaskHandoff } from "../features/knowledge/KnowledgeTaskHandoff";

export const consoleSurfaces = [
  { path: "/knowledge", label: "企业知识", icon: LibraryBig },
  { path: "/dream", label: "企业梦境", icon: MoonStar },
  { path: "/workflows", label: "做事流程", icon: Workflow },
  { path: "/evidence", label: "回答依据", icon: BookOpenCheck },
] as const;

const routeCopy = {
  knowledge: {
    title: "企业知识",
    purpose: "维护员工 Agent 可以使用的企业知识。",
    next: "从左侧选择知识范围，再查看或维护内容。",
  },
  dream: {
    title: "企业梦境",
    purpose: "查看系统从企业运行记录中自动整理出的组织知识。",
    next: "选择组织和时间，查看最近一次整理结果。",
  },
  workflows: {
    title: "做事流程",
    purpose: "维护企业中可以重复执行的 SOP 和工作流程。",
    next: "选择一个流程查看；需要修改时再进入编辑。",
  },
  evidence: {
    title: "回答依据",
    purpose: "核对员工 Agent 回答使用了哪些已授权知识。",
    next: "选择一条回答记录查看可读的依据说明。",
  },
} as const;

function RouteAdapter({ surface }: { surface: keyof typeof routeCopy }) {
  const copy = routeCopy[surface];
  return (
    <section className="console-route-intro" aria-labelledby={`${surface}-title`}>
      <p className="console-route-kicker">当前工作区</p>
      <h1 id={`${surface}-title`} className="title-display">
        {copy.title}
      </h1>
      <p>{copy.purpose}</p>
      <div className="glass-rest console-next-step">
        <strong>下一步</strong>
        <span>{copy.next}</span>
      </div>
    </section>
  );
}

function AssistantRoute() {
  const navigate = useNavigate();
  return (
    <div className="console-assistant-fullscreen">
      <DockedPanel
        open
        mode="fullscreen"
        title="Atlas 助手"
        onOpenChange={(open) => {
          if (!open) navigate(-1);
        }}
      >
        <p>告诉 Atlas 助手你想维护什么内容，它会引导你完成下一步。</p>
      </DockedPanel>
    </div>
  );
}

export function ConsoleRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/knowledge" replace />} />
      <Route path="/knowledge" element={<KnowledgeHome />} />
      <Route path="/knowledge/:orgUnitID" element={<KnowledgeHome />} />
      <Route path="/knowledge/:orgUnitID/:task" element={<KnowledgeTaskHandoff />} />
      <Route path="/dream/*" element={<RouteAdapter surface="dream" />} />
      <Route path="/workflows/*" element={<RouteAdapter surface="workflows" />} />
      <Route path="/evidence/*" element={<RouteAdapter surface="evidence" />} />
      <Route path="/assistant" element={<AssistantRoute />} />
      <Route path="/advanced/legacy/:surface" element={<LegacySurfaceRoute />} />
      <Route path="*" element={<Navigate to="/knowledge" replace />} />
    </Routes>
  );
}

function LegacySurfaceRoute() {
  const { surface } = useParams();
  if (!surface || !["knowledge", "dream", "workflows", "evidence", "assistant"].includes(surface)) {
    return <Navigate to="/knowledge" replace />;
  }
  return <LegacyRouteAdapter surface={surface as LegacySurface} />;
}
