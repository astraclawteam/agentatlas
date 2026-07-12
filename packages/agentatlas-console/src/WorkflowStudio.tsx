// 知识工作流工作台：给非技术维护者的编辑层。
// 左侧节点面板（16 种内置节点，中文名 + 一句人话说明）点击即追加；
// 步骤列表可删除；画布实时预览；保存草稿 / 发布走真实控制面 API。
import { useMemo, useState } from "react";
import { LegacyButton as ClawButton, LegacyInput as ClawInput } from "./app/runtime-ui-adapters";
import { AtlasWorkflowCanvas } from "./AtlasWorkflowCanvas";
import { createWorkflow, publishWorkflow } from "./api";
import type { AtlasNodeType, AtlasWorkflow, AtlasWorkflowNode } from "./types";

/** 16 种内置节点的人话说明 —— 维护者不需要理解英文类型名。 */
export const NODE_CATALOG: Array<{ type: AtlasNodeType; label: string; desc: string }> = [
  { type: "input.manual", label: "手动输入", desc: "流程的起点：接收管理员给的内容" },
  { type: "input.evidence_pointer", label: "选定资料", desc: "以库里的某份资料作为起点" },
  { type: "parser.document", label: "解析文档", desc: "把 PDF/Word/Markdown 拆成结构化内容" },
  { type: "parser.image", label: "解析图片", desc: "识别图片里的文字与版面" },
  { type: "parser.long_image", label: "解析长图", desc: "长截图分块识别并按阅读顺序拼回" },
  { type: "parser.audio", label: "解析音频", desc: "语音转文字（区分说话人）" },
  { type: "parser.video", label: "解析视频", desc: "拆分镜头、抽关键帧、转写声音" },
  { type: "transform.extract_sop", label: "抽取 SOP", desc: "让 Agent 从内容里整理出标准流程" },
  { type: "transform.summarize", label: "生成摘要", desc: "生成展示与检索用的摘要" },
  { type: "retrieval.search", label: "知识检索", desc: "在企业知识里搜索相关内容" },
  { type: "nexus.locate", label: "定位原文", desc: "找到原文存放的位置（走权限）" },
  { type: "nexus.read", label: "读取原文", desc: "在授权下读取原文片段" },
  { type: "human.confirm", label: "人工确认", desc: "停下来等管理员点确认再继续" },
  { type: "dream.aggregate", label: "汇总简报", desc: "把多条工作简报汇总成安全摘要" },
  { type: "answer.generate", label: "生成回答", desc: "根据检索到的证据生成回答" },
  { type: "trace.append", label: "记录追溯", desc: "把这次运行的证据链存档" },
];

const catalogByType = new Map(NODE_CATALOG.map((n) => [n.type, n]));

export function nodeLabel(type: string): string {
  return catalogByType.get(type as AtlasNodeType)?.label ?? type;
}

function emptyWorkflow(): AtlasWorkflow {
  return { workflow_id: "", version: 0, kind: "sop", nodes: [], edges: [], risk_level: "medium" };
}

/** 线性追加：新节点接在最后一个节点之后（覆盖“偶尔加几个节点”的日常维护）。
 *  id 序号取现有最大值 +1（不是 length+1）：删除中间步骤后再添加不会撞 id。 */
export function appendNode(wf: AtlasWorkflow, type: AtlasNodeType): AtlasWorkflow {
  const meta = catalogByType.get(type);
  const maxSeq = wf.nodes.reduce((max, n) => {
    const m = /^n(\d+)_/.exec(n.id);
    return m ? Math.max(max, Number(m[1])) : max;
  }, 0);
  const id = `n${maxSeq + 1}_${type.split(".")[1] ?? type}`;
  const node: AtlasWorkflowNode = {
    id,
    type,
    name: meta?.label ?? type,
    requires_confirmation: type === "human.confirm" ? true : undefined,
  };
  const edges = [...wf.edges];
  if (wf.nodes.length > 0) {
    edges.push({ from: wf.nodes[wf.nodes.length - 1].id, to: id });
  }
  return { ...wf, nodes: [...wf.nodes, node], edges };
}

/** 删除节点并把前后邻居重新连上（保持链路不断）。 */
export function removeNode(wf: AtlasWorkflow, id: string): AtlasWorkflow {
  const idx = wf.nodes.findIndex((n) => n.id === id);
  if (idx < 0) return wf;
  const prev = wf.nodes[idx - 1]?.id;
  const next = wf.nodes[idx + 1]?.id;
  const nodes = wf.nodes.filter((n) => n.id !== id);
  let edges = wf.edges.filter((e) => e.from !== id && e.to !== id);
  if (prev && next && !edges.some((e) => e.from === prev && e.to === next)) {
    edges = [...edges, { from: prev, to: next }];
  }
  return { ...wf, nodes, edges };
}

export interface WorkflowStudioProps {
  /** Agent 生成的草稿可直接载入编辑。 */
  initialWorkflow?: AtlasWorkflow | null;
}

export function WorkflowStudio({ initialWorkflow }: WorkflowStudioProps) {
  const [wf, setWf] = useState<AtlasWorkflow>(() => initialWorkflow ?? emptyWorkflow());
  const [name, setName] = useState("我的知识流程");
  const [savedID, setSavedID] = useState<string>(initialWorkflow?.workflow_id ?? "");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const revision = useMemo(() => JSON.stringify(wf.nodes.map((n) => n.id)), [wf]);

  const save = async () => {
    setBusy(true);
    setStatus("");
    try {
      const res = await createWorkflow(name, { ...wf, workflow_id: "", version: 0 });
      setSavedID(res.workflow_id);
      setStatus(`草稿已保存（${res.workflow_id}）。发布后才会生效。`);
    } catch (e) {
      setStatus(`保存失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const publish = async () => {
    if (!savedID) return;
    setBusy(true);
    setStatus("");
    try {
      const res = await publishWorkflow(savedID);
      setStatus(`已发布 第 ${res.version} 版（发布版不可修改；要改就存新草稿再发布）。`);
    } catch (e) {
      setStatus(`发布失败：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ display: "flex", height: "100%", minHeight: 0, fontFamily: "var(--claw-font)" }}>
      <aside style={{ width: 260, flexShrink: 0, overflowY: "auto", borderRight: "1px solid var(--claw-border)", padding: 12 }}>
        <div style={{ fontSize: 13, fontWeight: 600, color: "var(--claw-text)", marginBottom: 4 }}>添加步骤</div>
        <div style={{ fontSize: 12, color: "var(--claw-text-muted)", marginBottom: 10 }}>
          点一下就接到流程末尾，不满意随时删。
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {NODE_CATALOG.map((n) => (
            <button
              key={n.type}
              type="button"
              data-testid={`palette-${n.type}`}
              className="claw-tap-card"
              onClick={() => setWf((prev) => appendNode(prev, n.type))}
              style={{ padding: "8px 10px" }}
            >
              <div style={{ fontSize: 13, fontWeight: 600, color: "var(--claw-text)" }}>{n.label}</div>
              <div style={{ fontSize: 11, color: "var(--claw-text-muted)" }}>{n.desc}</div>
            </button>
          ))}
        </div>
      </aside>

      <div style={{ flex: 1, minWidth: 0, display: "flex", flexDirection: "column" }}>
        <div style={{ display: "flex", gap: 8, alignItems: "center", padding: "10px 14px", borderBottom: "1px solid var(--claw-border)" }}>
          <ClawInput aria-label="流程名称" value={name} onChange={(e) => setName(e.target.value)} style={{ width: 220 }} />
          <ClawButton onClick={save} disabled={busy || wf.nodes.length === 0}>保存草稿</ClawButton>
          <ClawButton variant="ghost" onClick={publish} disabled={busy || !savedID}>发布</ClawButton>
          <span data-testid="studio-status" style={{ fontSize: 12, color: "var(--claw-text-secondary)" }}>{status}</span>
        </div>

        {wf.nodes.length > 0 ? (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6, padding: "8px 14px", borderBottom: "1px solid var(--claw-border)" }}>
            {wf.nodes.map((n, i) => (
              <span
                key={n.id}
                style={{
                  display: "inline-flex", alignItems: "center", gap: 6, padding: "3px 8px",
                  fontSize: 12, border: "1px solid var(--claw-border)", borderRadius: 999,
                  background: "var(--claw-surface-solid)", color: "var(--claw-text-secondary)",
                }}
              >
                {i + 1}. {nodeLabel(n.type)}
                <button
                  type="button"
                  aria-label={`删除步骤 ${nodeLabel(n.type)}`}
                  onClick={() => setWf((prev) => removeNode(prev, n.id))}
                  style={{ border: "none", background: "transparent", cursor: "pointer", color: "var(--claw-text-muted)", padding: 0 }}
                >
                  ✕
                </button>
              </span>
            ))}
          </div>
        ) : null}

        <div style={{ flex: 1, minHeight: 0 }}>
          {wf.nodes.length === 0 ? (
            <div style={{ padding: 32, color: "var(--claw-text-muted)", fontSize: 13, lineHeight: 1.8 }}>
              这里编排「资料如何被解析、SOP 如何被执行、汇总如何生成」。
              <br />第一步：从左侧点一个「选定资料」或「手动输入」开始；
              <br />也可以直接对右侧 Atlas Agent 说“把这份文档做成 SOP”，草稿会出现在这里。
            </div>
          ) : (
            <AtlasWorkflowCanvas key={revision} workflow={{ ...wf, workflow_id: savedID || "draft" }} />
          )}
        </div>
      </div>
    </div>
  );
}
