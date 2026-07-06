package workflow

import (
	"strings"
	"testing"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

func validDef(id string) Definition {
	return Definition{
		WorkflowID: id,
		Version:    0,
		Kind:       sdkworkflow.KindSOP,
		Nodes: []sdkworkflow.Node{
			{ID: "in", Type: sdkworkflow.NodeInputManual},
			{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm},
			{ID: "out", Type: sdkworkflow.NodeInputEvidencePointer,
				Config: map[string]any{"evidence_pointer_id": "ev_1"}},
		},
		Edges: []sdkworkflow.Edge{
			{From: "in", To: "confirm"},
			{From: "confirm", To: "out"},
		},
		RiskLevel: sdkworkflow.RiskLow,
	}
}

func TestValidatorAcceptsValidDefinition(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	if err := v.Validate(validDef("wf_ok")); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
}

func TestValidatorRejectsUnknownNodeType(t *testing.T) {
	v, _ := NewValidator()
	def := validDef("wf_bad")
	def.Nodes[0].Type = "custom.hack"
	err := v.Validate(def)
	if err == nil || !strings.Contains(err.Error(), "schema violation") {
		t.Fatalf("unknown node type must be rejected, got %v", err)
	}
}

func TestValidatorRejectsMissingRiskLevel(t *testing.T) {
	v, _ := NewValidator()
	if err := v.ValidateBytes([]byte(`{
		"workflow_id":"wf_x","version":0,"kind":"sop",
		"nodes":[{"id":"a","type":"input.manual"}],"edges":[]
	}`)); err == nil {
		t.Fatal("missing risk_level must be rejected")
	}
}
