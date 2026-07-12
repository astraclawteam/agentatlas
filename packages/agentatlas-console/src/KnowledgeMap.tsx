// Read-only organization knowledge map (React Flow), grouped by scope kind.
import { useMemo } from "react";
import { ReactFlow, Background, type Node, type Edge } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { KnowledgeSpace } from "./types";

const KIND_ORDER: KnowledgeSpace["kind"][] = [
  "company",
  "business_unit",
  "department",
  "project_group",
  "employee",
];

const KIND_LABEL: Record<KnowledgeSpace["kind"], string> = {
  company: "公司",
  business_unit: "事业部",
  department: "部门",
  project_group: "项目组",
  employee: "员工",
};

export interface KnowledgeMapProps {
  spaces: KnowledgeSpace[];
  /** Optional explicit parent links: space_id -> parent space_id. */
  links?: Array<{ from: string; to: string }>;
  onSelect?: (space: KnowledgeSpace) => void;
}

export function KnowledgeMap({ spaces, links = [], onSelect }: KnowledgeMapProps) {
  const { nodes, edges } = useMemo(() => {
    const nodes: Node[] = spaces.map((s) => {
      const col = KIND_ORDER.indexOf(s.kind);
      const siblings = spaces.filter((x) => x.kind === s.kind);
      const row = siblings.findIndex((x) => x.space_id === s.space_id);
      return {
        id: s.space_id,
        position: { x: col * 240, y: row * 90 },
        data: { label: `${KIND_LABEL[s.kind]} · ${s.name}` },
        draggable: false,
        connectable: false,
        style: {
          fontFamily: "var(--claw-font)",
          fontSize: 13,
          border: "1px solid var(--claw-border-strong)",
          borderRadius: 12,
          padding: "8px 12px",
          background: "var(--claw-surface-solid)",
        },
      };
    });
    const edges: Edge[] = links.map((l) => ({
      id: `${l.from}->${l.to}`,
      source: l.from,
      target: l.to,
    }));
    return { nodes, edges };
  }, [spaces, links]);

  if (spaces.length === 0) {
    return (
      <div style={{ padding: 32, fontFamily: "var(--claw-font)", color: "var(--claw-text-muted)" }}>
        暂无知识空间。组织图同步（AgentNexus org events）后自动创建。
      </div>
    );
  }

  return (
    <section
      className="knowledge-map-readonly"
      data-testid="knowledge-map"
      aria-label="只读组织知识图"
      aria-describedby="knowledge-map-description"
    >
      <p id="knowledge-map-description" className="knowledge-map-description">
        这里仅用于查看组织知识之间的关系；修改内容请返回企业知识列表。
      </p>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        fitView
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        onNodeClick={(_, node) => {
          const space = spaces.find((s) => s.space_id === node.id);
          if (space && onSelect) onSelect(space);
        }}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={24} />
      </ReactFlow>
    </section>
  );
}
