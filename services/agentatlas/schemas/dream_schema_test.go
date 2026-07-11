package schemas

import "testing"

const validDreamPolicy = `{
  "org_unit_id":"dept-rd",
  "timezone":"Asia/Shanghai",
  "schedule":"0 22 * * *",
  "input_sources":["work_brief","child_dream_summary"],
  "workflow":{"id":"wf-dream","version":3},
  "output_space_id":"space-rd",
  "visibility_level":"managers",
  "confirmation_mode":"high_risk_only"
}`

func TestValidateDreamPolicyMatrix(t *testing.T) {
	if err := ValidateDreamPolicy([]byte(validDreamPolicy)); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	invalid := map[string]string{
		"unknown timezone":      `{"org_unit_id":"dept-rd","timezone":"Mars/Olympus","schedule":"0 22 * * *","input_sources":["work_brief"],"workflow":{"id":"wf","version":1},"output_space_id":"space","visibility_level":"managers","confirmation_mode":"high_risk_only"}`,
		"invalid cron":          `{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"tomorrow","input_sources":["work_brief"],"workflow":{"id":"wf","version":1},"output_space_id":"space","visibility_level":"managers","confirmation_mode":"high_risk_only"}`,
		"duplicate sources":     `{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"0 22 * * *","input_sources":["work_brief","work_brief"],"workflow":{"id":"wf","version":1},"output_space_id":"space","visibility_level":"managers","confirmation_mode":"high_risk_only"}`,
		"empty workflow id":     `{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"0 22 * * *","input_sources":["work_brief"],"workflow":{"id":"","version":1},"output_space_id":"space","visibility_level":"managers","confirmation_mode":"high_risk_only"}`,
		"zero workflow version": `{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"0 22 * * *","input_sources":["work_brief"],"workflow":{"id":"wf","version":0},"output_space_id":"space","visibility_level":"managers","confirmation_mode":"high_risk_only"}`,
	}
	for name, doc := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDreamPolicy([]byte(doc)); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestValidateDreamRunRejectsRawStructuredData(t *testing.T) {
	valid := []byte(`{
      "run_id":"run-1","status":"succeeded","org_unit_id":"dept-rd",
      "window_start":"2026-07-10T00:00:00Z","window_end":"2026-07-11T00:00:00Z",
      "policy_version":1,"workflow":{"id":"wf-dream","version":3},
      "parent_run_ids":[],"input_count":1,
      "coverage":{"expected_children":1,"completed_children":1,"input_count":1},
      "missing_inputs":[],"facts":[{"id":"f-1","title":"fact","detail":"detail","severity":"info","evidence_pointer_id":"ev-1"}],"themes":[],"trends":[],"risks":[],"todos":[],
      "display_summary":"done","evidence_pointer_id":"ev-1",
      "input_snapshot":{"source_counts":[{"source_type":"work_brief","count":1}],"sanitized_input_ids":["input-1"]},
      "visibility_snapshot":{"visibility_level":"managers","org_unit_ids":["dept-rd"],"masked_field_count":0},
      "model_route":"reasoning","model_version":"v1","attempt":1,"idempotency_key":"idem-1"
    }`)
	if err := ValidateDreamRun(valid); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
	if err := ValidateDreamSummary([]byte(`{"display_summary":"d","retrieval_summary":"r","sealed_detail_pointer":"s3://sealed","facts":[],"themes":[],"trends":[],"risks":[],"todos":[],"coverage":{"expected_children":0,"completed_children":0,"input_count":0},"missing_inputs":[],"evidence_pointer_id":"ev-1"}`)); err != nil {
		t.Fatalf("valid summary rejected: %v", err)
	}
	doc := []byte(`{
      "run_id":"run-1","status":"succeeded","org_unit_id":"dept-rd",
      "window_start":"2026-07-10T00:00:00Z","window_end":"2026-07-11T00:00:00Z",
      "policy_version":1,"workflow":{"id":"wf-dream","version":3},
      "parent_run_ids":[],"input_count":1,
      "coverage":{"expected_children":1,"completed_children":1,"input_count":1},
      "missing_inputs":[],"facts":[{"raw":"unbounded"}],"themes":[],"trends":[],"risks":[],"todos":[],
      "display_summary":"done","evidence_pointer_id":"ev-1"
    }`)
	if err := ValidateDreamRun([]byte(doc)); err == nil {
		t.Fatal("untyped structured signal accepted")
	}
}

func TestValidateDreamRunRejectsInvalidDateTimeFormat(t *testing.T) {
	doc := []byte(`{"run_id":"run-1","status":"succeeded","org_unit_id":"dept-rd","window_start":"yesterday","window_end":"tomorrow","policy_version":1,"workflow":{"id":"wf-dream","version":3},"parent_run_ids":[],"input_count":0,"coverage":{"expected_children":0,"completed_children":0,"input_count":0},"missing_inputs":[],"facts":[],"themes":[],"trends":[],"risks":[],"todos":[],"display_summary":"done","evidence_pointer_id":"ev-1","input_snapshot":{"source_counts":[],"sanitized_input_ids":[]},"visibility_snapshot":{"visibility_level":"managers","org_unit_ids":["dept-rd"],"masked_field_count":0},"model_route":"reasoning","model_version":"v1","attempt":1,"idempotency_key":"idem-1"}`)
	if err := ValidateDreamRun(doc); err == nil {
		t.Fatal("invalid DreamRun date-time accepted")
	}
}
