package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
)

type fakePipeline struct{ docID string }

func (f fakePipeline) ParseArtifact(_ context.Context, _, artifactID, _ string) (string, error) {
	if artifactID == "art_bad" {
		return "", fmt.Errorf("parse exploded")
	}
	return f.docID, nil
}

type fakeDocs struct{}

func (fakeDocs) LoadSOPDocument(context.Context, string) (agent.SOPDocument, error) {
	return agent.SOPDocument{Title: "MES SOP", Sections: []string{"第一步"}}, nil
}

func (fakeDocs) LoadParseOutput(context.Context, string) (parsergateway.ParseOutput, error) {
	return parsergateway.ParseOutput{ProviderID: "docling"}, nil
}

type fakeRetriever struct{}

func (fakeRetriever) CreatePlan(context.Context, retrieval.Query) (string, error) {
	return "rp_1", nil
}

func (fakeRetriever) Execute(context.Context, string, retrieval.Query) ([]retrieval.Result, error) {
	return []retrieval.Result{{DocID: "d1", Snippet: "MES 摘要", EvidencePointerID: "ev_1", Score: 1.5}}, nil
}

func execNode(t *testing.T, r Registry, node sdkworkflow.Node, run *RunContext) map[string]any {
	t.Helper()
	out, err := r[node.Type].Execute(context.Background(), node, run)
	if err != nil {
		t.Fatalf("execute %s: %v", node.Type, err)
	}
	return out
}

func TestExecutorsCoverAllSixteenNodeTypes(t *testing.T) {
	r := NewRegistryWithServices(Executors{
		Artifacts: fakePipeline{docID: "doc_1"},
		Documents: fakeDocs{},
		SOP: func(_ context.Context, doc agent.SOPDocument) (sdkworkflow.Workflow, error) {
			return sdkworkflow.Workflow{WorkflowID: "wf_gen", Kind: sdkworkflow.KindSOP,
				Nodes: []sdkworkflow.Node{{ID: "a", Type: sdkworkflow.NodeInputManual}},
				Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}, nil
		},
		Retrieval: fakeRetriever{},
		Nexus:     nexusclient.NewMock(),
		Dream: func(_ context.Context, scope string, texts []string, _, _ []string) (map[string]any, error) {
			return map[string]any{"display": scope + fmt.Sprint(len(texts)), "source": "deterministic"}, nil
		},
		Answer: func(_ context.Context, q string, snips []string) (string, string, error) {
			return "答案:" + q, "test/model", nil
		},
	})
	for _, typ := range sdkworkflow.BuiltinNodeTypes {
		if _, ok := r[typ]; !ok {
			t.Fatalf("node type %s missing from service registry", typ)
		}
	}
	// no notWired executor remains: probing an unconfigured dep must complain
	// about configuration, never about "not wired".
	for typ, exec := range r {
		if typ == sdkworkflow.NodeHumanConfirm {
			continue // runtime pause point
		}
		_, err := exec.Execute(context.Background(), sdkworkflow.Node{ID: "probe", Type: typ}, &RunContext{
			Input: map[string]any{}, Outputs: map[string]map[string]any{},
		})
		if err != nil && strings.Contains(err.Error(), "not wired") {
			t.Fatalf("node type %s still notWired", typ)
		}
	}
}

func TestParserExecutorFlowsArtifactToDocument(t *testing.T) {
	r := NewRegistryWithServices(Executors{Artifacts: fakePipeline{docID: "doc_9"}})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "art_1"}, Outputs: map[string]map[string]any{}}
	out := execNode(t, r, sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, run)
	if out["atlas_document_id"] != "doc_9" {
		t.Fatalf("out = %v", out)
	}
	// failures propagate loud
	badRun := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "art_bad"}, Outputs: map[string]map[string]any{}}
	if _, err := r[sdkworkflow.NodeParserDocument].Execute(context.Background(), sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, badRun); err == nil {
		t.Fatal("parser failure must fail loud")
	}
}

func TestRetrievalAndAnswerExecutorsChainOutputs(t *testing.T) {
	r := NewRegistryWithServices(Executors{
		Retrieval: fakeRetriever{},
		Answer: func(_ context.Context, q string, snips []string) (string, string, error) {
			return fmt.Sprintf("基于 %d 条材料回答:%s", len(snips), q), "test/model", nil
		},
	})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"query": "我的工作", "question": "我的工作"}, Outputs: map[string]map[string]any{}}
	search := execNode(t, r, sdkworkflow.Node{ID: "s", Type: sdkworkflow.NodeRetrievalSearch}, run)
	if search["plan_id"] != "rp_1" {
		t.Fatalf("search out = %v", search)
	}
	run.Outputs["s"] = search
	answer := execNode(t, r, sdkworkflow.Node{ID: "a", Type: sdkworkflow.NodeAnswerGenerate}, run)
	if !strings.Contains(answer["answer"].(string), "基于 1 条材料") {
		t.Fatalf("answer must consume upstream snippets: %v", answer)
	}
}

func TestNexusExecutorsFailClosedWithoutTicket(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Locations["ev_1"] = nexus.LocateEvidenceResponse{ResourceURI: "fs://x", SourceSystem: "filesystem"}
	r := NewRegistryWithServices(Executors{Nexus: mock})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"evidence_pointer_id": "ev_1"}, Outputs: map[string]map[string]any{}}
	if _, err := r[sdkworkflow.NodeNexusLocate].Execute(context.Background(), sdkworkflow.Node{ID: "l", Type: sdkworkflow.NodeNexusLocate}, run); err == nil || !strings.Contains(err.Error(), "ticket_id") {
		t.Fatalf("nexus.locate without ticket must fail closed, got %v", err)
	}
	run.Input["ticket_id"] = "tick_1"
	out := execNode(t, r, sdkworkflow.Node{ID: "l", Type: sdkworkflow.NodeNexusLocate}, run)
	if out["resource_uri"] != "fs://x" {
		t.Fatalf("locate out = %v", out)
	}
}

func TestUnconfiguredExecutorDependencyFailsLoud(t *testing.T) {
	r := NewRegistryWithServices(Executors{}) // nothing configured
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "a"}, Outputs: map[string]map[string]any{}}
	_, err := r[sdkworkflow.NodeParserDocument].Execute(context.Background(), sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, run)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured dep must fail loud, got %v", err)
	}
}
