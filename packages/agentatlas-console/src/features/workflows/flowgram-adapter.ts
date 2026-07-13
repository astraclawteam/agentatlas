import type { WorkflowJSON } from "@flowgram.ai/free-layout-editor";

import type { AtlasWorkflow, AtlasWorkflowEdge, AtlasWorkflowNode } from "../../types";

const ATLAS = "agentatlas";

type FlowNode = WorkflowJSON["nodes"][number] & { data?: Record<string, unknown> };
type FlowEdge = WorkflowJSON["edges"][number] & { data?: Record<string, unknown> };

interface AtlasNodeEnvelope {
  node: AtlasWorkflowNode;
  workflow?: Omit<AtlasWorkflow, "nodes" | "edges">;
}

interface AtlasEdgeEnvelope { edge: AtlasWorkflowEdge }

function clone<T>(value: T): T {
  return value === undefined ? value : structuredClone(value);
}

/** Layout is derived, while canonical business fields are preserved under a namespace. */
export function toWorkflowJSON(workflow: AtlasWorkflow): WorkflowJSON {
  const depth = new Map<string, number>();
  const incoming = new Map(workflow.nodes.map((node) => [node.id, 0]));
  for (const edge of workflow.edges) incoming.set(edge.to, (incoming.get(edge.to) ?? 0) + 1);
  const queue = workflow.nodes.filter((node) => incoming.get(node.id) === 0).map((node) => node.id);
  queue.forEach((id) => depth.set(id, 0));
  for (let cursor = 0; cursor < queue.length; cursor += 1) {
    const id = queue[cursor];
    for (const edge of workflow.edges.filter((item) => item.from === id)) {
      const next = Math.max(depth.get(edge.to) ?? 0, (depth.get(id) ?? 0) + 1);
      if (depth.get(edge.to) !== next) {
        depth.set(edge.to, next);
        queue.push(edge.to);
      }
    }
  }
  const rows = new Map<number, number>();
  const workflowMeta: Omit<AtlasWorkflow, "nodes" | "edges"> = {
    workflow_id: workflow.workflow_id,
    version: workflow.version,
    kind: workflow.kind,
    risk_level: workflow.risk_level,
    ...(workflow.variables === undefined ? {} : { variables: clone(workflow.variables) }),
    ...(workflow.input_schema === undefined ? {} : { input_schema: clone(workflow.input_schema) }),
    ...(workflow.output_schema === undefined ? {} : { output_schema: clone(workflow.output_schema) }),
  };
  return {
    nodes: workflow.nodes.map((node, index) => {
      const column = depth.get(node.id) ?? 0;
      const row = rows.get(column) ?? 0;
      rows.set(column, row + 1);
      return {
        id: node.id,
        type: node.type,
        meta: { position: { x: 80 + column * 260, y: 80 + row * 130 } },
        data: {
          title: node.name || node.type,
          atlasType: node.type,
          requiresConfirmation: node.requires_confirmation ?? false,
          [ATLAS]: { node: clone(node), ...(index === 0 ? { workflow: workflowMeta } : {}) } satisfies AtlasNodeEnvelope,
        },
      };
    }),
    edges: workflow.edges.map((edge) => ({
      sourceNodeID: edge.from,
      targetNodeID: edge.to,
      data: { [ATLAS]: { edge: clone(edge) } satisfies AtlasEdgeEnvelope },
    })),
  };
}

export function fromWorkflowJSON(json: WorkflowJSON, fallback: AtlasWorkflow): AtlasWorkflow {
  const nodes = (json.nodes as FlowNode[]).map((node) => {
    const envelope = node.data?.[ATLAS] as AtlasNodeEnvelope | undefined;
    const original = envelope?.node;
    const confirmation = node.data?.requiresConfirmation;
    const confirmationFields = typeof confirmation === "boolean" && (confirmation || original?.requires_confirmation !== undefined)
      ? { requires_confirmation: confirmation }
      : {};
    return {
      ...(original ? clone(original) : { id: String(node.id), type: String(node.data?.atlasType ?? node.type) }),
      id: String(node.id),
      type: String(node.data?.atlasType ?? original?.type ?? node.type),
      ...(typeof node.data?.title === "string" ? { name: node.data.title } : {}),
      ...confirmationFields,
    } satisfies AtlasWorkflowNode;
  });
  const edges = (json.edges as FlowEdge[]).map((edge) => {
    const original = (edge.data?.[ATLAS] as AtlasEdgeEnvelope | undefined)?.edge;
    return {
      ...(original ? clone(original) : {}),
      from: String(edge.sourceNodeID),
      to: String(edge.targetNodeID),
    } satisfies AtlasWorkflowEdge;
  });
  const stored = ((json.nodes[0] as FlowNode | undefined)?.data?.[ATLAS] as AtlasNodeEnvelope | undefined)?.workflow;
  return { ...clone(fallback), ...(stored ? clone(stored) : {}), nodes, edges };
}

const BASIC_TYPES = new Set(["input.manual", "input.evidence_pointer", "human.confirm", "transform.extract_sop", "transform.summarize"]);

export function isBasicLinearWorkflow(workflow: AtlasWorkflow): boolean {
  if (workflow.kind !== "sop" || workflow.nodes.some((node) => !BASIC_TYPES.has(node.type)) || workflow.edges.some((edge) => Boolean(edge.condition))) return false;
  if (workflow.nodes.length === 0) return true;
  if (workflow.edges.length !== workflow.nodes.length - 1) return false;
  const incoming = new Map(workflow.nodes.map((node) => [node.id, 0]));
  const outgoing = new Map(workflow.nodes.map((node) => [node.id, 0]));
  for (const edge of workflow.edges) {
    if (!incoming.has(edge.to) || !outgoing.has(edge.from)) return false;
    incoming.set(edge.to, incoming.get(edge.to)! + 1);
    outgoing.set(edge.from, outgoing.get(edge.from)! + 1);
  }
  return [...incoming.values()].filter((count) => count === 0).length === 1
    && [...outgoing.values()].filter((count) => count === 0).length === 1
    && [...incoming.values()].every((count) => count <= 1)
    && [...outgoing.values()].every((count) => count <= 1);
}

export function linearNodeOrder(workflow: AtlasWorkflow): AtlasWorkflowNode[] | null {
  if (!isBasicLinearWorkflow(workflow)) return null;
  if (!workflow.nodes.length) return [];
  const next = new Map(workflow.edges.map((edge) => [edge.from, edge.to]));
  const destinations = new Set(workflow.edges.map((edge) => edge.to));
  let cursor = workflow.nodes.find((node) => !destinations.has(node.id));
  const ordered: AtlasWorkflowNode[] = [];
  while (cursor) {
    ordered.push(cursor);
    const nextID = next.get(cursor.id);
    cursor = nextID ? workflow.nodes.find((node) => node.id === nextID) : undefined;
  }
  return ordered.length === workflow.nodes.length ? ordered : null;
}
