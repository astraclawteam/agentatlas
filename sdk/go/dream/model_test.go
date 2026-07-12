package dream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicDreamModelsUseCanonicalNestedTypes(t *testing.T) {
	view := DreamRunView{
		Workflow: WorkflowRef{ID: "wf-dream", Version: 3},
		Coverage: Coverage{ExpectedChildren: 2, CompletedChildren: 1, InputCount: 4},
		Facts:    []StructuredSignal{{ID: "fact-1", Title: "fact", Detail: "detail", Severity: SignalSeverityInfo, EvidencePointerID: "ev-1"}},
	}
	if view.Workflow.Version != 3 || view.Facts[0].EvidencePointerID == "" {
		t.Fatal("canonical typed model lost data")
	}
}

func TestDreamPolicyPartialChildrenIsExplicitAndDefaultsSafe(t *testing.T) {
	var zero DreamPolicyDefinition
	if zero.AllowPartialChildren {
		t.Fatal("partial child failures must default disabled")
	}
	raw, err := json.Marshal(DreamPolicyDefinition{AllowPartialChildren: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"allow_partial_children":true`) {
		t.Fatalf("public contract lost partial-child permission: %s", raw)
	}
}
