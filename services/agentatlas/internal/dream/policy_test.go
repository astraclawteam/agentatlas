package dream

import (
	"encoding/json"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

func validPolicy() Policy {
	return Policy(sdkdream.DreamPolicyDefinition{
		OrgUnitID: "department:rd-1", Timezone: "Asia/Shanghai", Schedule: "0 22 * * *",
		InputSources: []sdkdream.Source{sdkdream.SourceWorkBrief},
		Workflow:     sdkdream.WorkflowRef{ID: "wf-dream", Version: 3}, VisibilityLevel: sdkdream.VisibilityMembers,
		MaskingRules: []string{`1[3-9]\d{9}`}, RiskSignalRules: []string{`risk:\S+`},
		EvidenceRetention: sdkdream.EvidencePointerPlusDisplaySummary, OutputSpaceID: "spc_dept",
		ConfirmationMode: sdkdream.ConfirmationHighRiskOnly,
	})
}

func TestPolicyPublishValidatesExactPublishedDreamWorkflow(t *testing.T) {
	p := validPolicy()
	def := sdkworkflow.Workflow{WorkflowID: p.Workflow.ID, Version: int(p.Workflow.Version), Kind: sdkworkflow.KindDream, RiskLevel: sdkworkflow.RiskLow, Nodes: []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}}, Edges: []sdkworkflow.Edge{{From: "aggregate", To: "trace"}}}
	raw, _ := json.Marshal(def)
	if err := validatePublishedDreamWorkflow("ent-1", p, db.Workflow{ID: p.Workflow.ID, EnterpriseID: "ent-1", Kind: "dream"}, db.WorkflowVersion{WorkflowID: p.Workflow.ID, Version: p.Workflow.Version, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*db.Workflow, *db.WorkflowVersion){
		"cross_enterprise": func(w *db.Workflow, _ *db.WorkflowVersion) { w.EnterpriseID = "ent-2" },
		"wrong_kind":       func(w *db.Workflow, _ *db.WorkflowVersion) { w.Kind = "sop" },
		"wrong_version":    func(_ *db.Workflow, v *db.WorkflowVersion) { v.Version++ },
		"missing_aggregate": func(_ *db.Workflow, v *db.WorkflowVersion) {
			d := def
			d.Nodes = nil
			v.Definition, _ = json.Marshal(d)
		},
		"ambiguous_aggregate": func(_ *db.Workflow, v *db.WorkflowVersion) {
			d := def
			d.Nodes = append(d.Nodes, sdkworkflow.Node{ID: "aggregate2", Type: sdkworkflow.NodeDreamAggregate})
			v.Definition, _ = json.Marshal(d)
		},
		"missing_trace": func(_ *db.Workflow, v *db.WorkflowVersion) {
			d := def
			d.Nodes = d.Nodes[:1]
			d.Edges = nil
			v.Definition, _ = json.Marshal(d)
		},
		"unreachable_trace": func(_ *db.Workflow, v *db.WorkflowVersion) {
			d := def
			d.Edges = nil
			v.Definition, _ = json.Marshal(d)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := db.Workflow{ID: p.Workflow.ID, EnterpriseID: "ent-1", Kind: "dream"}
			v := db.WorkflowVersion{WorkflowID: p.Workflow.ID, Version: p.Workflow.Version, Definition: raw}
			mutate(&w, &v)
			if validatePublishedDreamWorkflow("ent-1", p, w, v) == nil {
				t.Fatal("must reject")
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := validPolicy().Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	bad := validPolicy()
	bad.Schedule = "not-cron"
	if bad.Validate() == nil {
		t.Fatal("bad cron must fail")
	}
	bad = validPolicy()
	bad.VisibilityLevel = "everyone"
	if bad.Validate() == nil {
		t.Fatal("unknown visibility must fail")
	}
	bad = validPolicy()
	bad.MaskingRules = []string{"("}
	if bad.Validate() == nil {
		t.Fatal("bad masking regex must fail")
	}
	bad = validPolicy()
	bad.InputSources = []sdkdream.Source{"telepathy"}
	if bad.Validate() == nil {
		t.Fatal("unknown input source must fail")
	}
}

func TestDecodePolicySupportsCanonicalAndLegacyDrafts(t *testing.T) {
	canonical, err := decodePolicy([]byte(`{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"0 22 * * *","input_sources":["work_brief"],"workflow":{"id":"wf-dream","version":3},"output_space_id":"space-rd","visibility_level":"managers","confirmation_mode":"high_risk_only"}`))
	if err != nil {
		t.Fatal(err)
	}
	if canonical.OrgUnitID != "dept-rd" || canonical.Workflow.ID != "wf-dream" {
		t.Fatalf("canonical decode = %+v", canonical)
	}
	legacy, err := decodePolicy([]byte(`{"org_scope":"department:legacy","schedule":"0 22 * * *","input_sources":["work_briefs"],"visibility_level":"members","evidence_retention":"pointer_only","output_space_id":"space-old"}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.OrgUnitID != "department:legacy" || len(legacy.InputSources) != 1 || legacy.InputSources[0] != sdkdream.SourceWorkBrief {
		t.Fatalf("legacy migration = %+v", legacy)
	}
}

func TestDueComputesWindow(t *testing.T) {
	p := validPolicy()
	now := time.Date(2026, 7, 6, 23, 0, 0, 0, time.UTC)
	start, end, due, err := Due(p, time.Time{}, now)
	if err != nil || !due {
		t.Fatalf("due: %v %v", due, err)
	}
	if end != time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC) {
		t.Fatalf("end = %v", end)
	}
	if start != end.Add(-24*time.Hour) {
		t.Fatalf("first window start = %v", start)
	}
	_, _, due, err = Due(p, end, now)
	if err != nil || due {
		t.Fatalf("must not be due again: %v %v", due, err)
	}
	nextNow := now.Add(24 * time.Hour)
	start2, end2, due2, _ := Due(p, end, nextNow)
	if !due2 || start2 != end || end2 != end.Add(24*time.Hour) {
		t.Fatalf("second window: %v %v due=%v", start2, end2, due2)
	}
}
