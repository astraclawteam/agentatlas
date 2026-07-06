// Package workflow mirrors schemas/workflow/workflow.schema.json. Enterprise
// extensions build workflow documents against these types; authoritative
// validation happens server-side against the JSON Schema.
package workflow

// Kind of a workflow document.
type Kind string

const (
	KindSOP       Kind = "sop"
	KindDream     Kind = "dream"
	KindIngestion Kind = "ingestion"
	KindAnswer    Kind = "answer"
)

// RiskLevel drives publish rules: high-risk workflows always require human
// confirmation before publish.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// NodeType is one of the sixteen built-in node types.
type NodeType string

const (
	NodeInputManual          NodeType = "input.manual"
	NodeInputEvidencePointer NodeType = "input.evidence_pointer"
	NodeParserDocument       NodeType = "parser.document"
	NodeParserImage          NodeType = "parser.image"
	NodeParserLongImage      NodeType = "parser.long_image"
	NodeParserAudio          NodeType = "parser.audio"
	NodeParserVideo          NodeType = "parser.video"
	NodeTransformExtractSOP  NodeType = "transform.extract_sop"
	NodeTransformSummarize   NodeType = "transform.summarize"
	NodeRetrievalSearch      NodeType = "retrieval.search"
	NodeNexusLocate          NodeType = "nexus.locate"
	NodeNexusRead            NodeType = "nexus.read"
	NodeHumanConfirm         NodeType = "human.confirm"
	NodeDreamAggregate       NodeType = "dream.aggregate"
	NodeAnswerGenerate       NodeType = "answer.generate"
	NodeTraceAppend          NodeType = "trace.append"
)

// BuiltinNodeTypes lists every node type the open-core runtime executes.
var BuiltinNodeTypes = []NodeType{
	NodeInputManual,
	NodeInputEvidencePointer,
	NodeParserDocument,
	NodeParserImage,
	NodeParserLongImage,
	NodeParserAudio,
	NodeParserVideo,
	NodeTransformExtractSOP,
	NodeTransformSummarize,
	NodeRetrievalSearch,
	NodeNexusLocate,
	NodeNexusRead,
	NodeHumanConfirm,
	NodeDreamAggregate,
	NodeAnswerGenerate,
	NodeTraceAppend,
}

func IsBuiltinNodeType(t NodeType) bool {
	for _, k := range BuiltinNodeTypes {
		if k == t {
			return true
		}
	}
	return false
}

type Node struct {
	ID                   string         `json:"id"`
	Type                 NodeType       `json:"type"`
	Name                 string         `json:"name,omitempty"`
	Config               map[string]any `json:"config,omitempty"`
	RequiresConfirmation bool           `json:"requires_confirmation,omitempty"`
}

type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

type Workflow struct {
	WorkflowID   string         `json:"workflow_id"`
	Version      int            `json:"version"`
	Kind         Kind           `json:"kind"`
	Nodes        []Node         `json:"nodes"`
	Edges        []Edge         `json:"edges"`
	Variables    map[string]any `json:"variables,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	RiskLevel    RiskLevel      `json:"risk_level"`
}
