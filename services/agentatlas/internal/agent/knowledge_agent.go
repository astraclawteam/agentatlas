package agent

import (
	"fmt"

	adk "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
)

const knowledgeInstructionTemplate = `你是 AgentAtlas 的 Knowledge Agent，负责企业知识空间、SOP 工作流、梦境策略与回答追溯。

硬性规则：
1. 企业原文只能经 AgentNexus 授权读取，绝不绕过；没有有效 ticket 时拒绝回答企业资源相关问题。
2. 回答必须落在证据上：引用知识空间、摘要与证据指针，不得虚构来源。
3. 工作流草稿只使用这 16 种内置节点类型（step.type 必须逐字取自本清单）：%s。发布永远需要管理员确认，你只产出草稿。
4. 梦境策略必须声明可见性等级与脱敏规则，公司级汇总默认脱敏。
5. 不确定时明确说不确定，并说明需要补充的证据。

用中文回答。需要生成工作流草稿时调用 draft_workflow，需要检索时调用 draft_retrieval_plan，解释回答来源时调用 explain_answer_trace。`

// NewKnowledgeAgent wires the Knowledge Agent on top of any ADK model.LLM
// (production: the adk-llmrouter-model adapter).
func NewKnowledgeAgent(llm model.LLM) (adk.Agent, error) {
	tools, err := Tools()
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:        "knowledge_agent",
		Description: "企业知识空间、SOP 工作流、梦境策略与回答追溯的知识中枢 Agent",
		Model:       llm,
		Instruction: fmt.Sprintf(knowledgeInstructionTemplate, builtinNodeTypeList()),
		Tools:       tools,
	})
}
