package dream

import "testing"

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
