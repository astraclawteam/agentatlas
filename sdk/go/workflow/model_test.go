package workflow

import (
	"encoding/json"
	"testing"
)

func TestBuiltinNodeTypes(t *testing.T) {
	if len(BuiltinNodeTypes) != 16 {
		t.Fatalf("builtin node types = %d, want 16", len(BuiltinNodeTypes))
	}
	if !IsBuiltinNodeType(NodeHumanConfirm) {
		t.Fatal("human.confirm must be builtin")
	}
	if IsBuiltinNodeType("custom.thing") {
		t.Fatal("custom.thing must not be builtin")
	}
}

func TestWorkflowJSONFieldNames(t *testing.T) {
	w := Workflow{
		WorkflowID: "wf_1",
		Version:    1,
		Kind:       KindSOP,
		Nodes:      []Node{{ID: "n1", Type: NodeInputManual}},
		Edges:      []Edge{{From: "n1", To: "n1"}},
		RiskLevel:  RiskLow,
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"workflow_id", "version", "kind", "nodes", "edges", "risk_level"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing schema field %q in %s", key, raw)
		}
	}
}
