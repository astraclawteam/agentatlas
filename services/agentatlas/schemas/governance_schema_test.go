package schemas

import "testing"

func TestValidateReviewRouteMatrix(t *testing.T) {
	if err := ValidateRiskAssessment([]byte(`{"risk_level":"high","risk_reasons":["workflow binding changed"]}`)); err != nil {
		t.Fatalf("valid risk assessment rejected: %v", err)
	}
	valid := map[string]string{
		"single low no reviewer": `{"change_id":"chg-1","resource_type":"knowledge_entry","resource_id":"k-1","requester_user_id":"u-1","risk_level":"low","mode":"single_confirmation","state":"pending","org_path":[]}`,
		"upward high reviewer":   `{"change_id":"chg-2","resource_type":"dream_policy","resource_id":"p-1","requester_user_id":"u-1","risk_level":"high","mode":"upward_review","state":"pending","reviewer_user_id":"u-2","org_path":["dept-rd","company"]}`,
		"admin high queue":       `{"change_id":"chg-3","resource_type":"workflow","resource_id":"wf-1","requester_user_id":"u-1","risk_level":"high","mode":"enterprise_knowledge_admin_queue","state":"pending","org_path":[],"queue":"knowledge-admins"}`,
	}
	for name, doc := range valid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateReviewRoute([]byte(doc)); err != nil {
				t.Fatalf("valid route rejected: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"medium risk":        `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","risk_level":"medium","mode":"single_confirmation","state":"pending","org_path":[]}`,
		"single reviewer":    `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","reviewer_user_id":"u2","risk_level":"low","mode":"single_confirmation","state":"pending","org_path":[]}`,
		"upward low":         `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","reviewer_user_id":"u2","risk_level":"low","mode":"upward_review","state":"pending","org_path":["company"]}`,
		"upward no reviewer": `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","risk_level":"high","mode":"upward_review","state":"pending","org_path":["company"]}`,
		"upward empty path":  `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","reviewer_user_id":"u2","risk_level":"high","mode":"upward_review","state":"pending","org_path":[]}`,
		"self review":        `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","reviewer_user_id":"u1","risk_level":"high","mode":"upward_review","state":"pending","org_path":["company"]}`,
		"admin low":          `{"change_id":"chg","resource_type":"workflow","resource_id":"wf","requester_user_id":"u1","risk_level":"low","mode":"enterprise_knowledge_admin_queue","state":"pending","org_path":[],"queue":"admins"}`,
	}
	for name, doc := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateReviewRoute([]byte(doc)); err == nil {
				t.Fatal("invalid route accepted")
			}
		})
	}
}

func TestValidateChangeDraftSuggestionCannotPublish(t *testing.T) {
	doc := []byte(`{
      "change_id":"chg-1","enterprise_id":"ent-1","org_unit_id":"dept-rd",
      "resource_type":"knowledge_entry","resource_id":"k-1","action":"publish",
      "requester_user_id":"employee-1","origin":"employee_suggestion","permission_mode":"suggestion_only",
      "revision":1,"state":"draft","base_version":0,"proposed_content":{},
      "created_at":"2026-07-11T00:00:00Z","updated_at":"2026-07-11T00:00:00Z"
    }`)
	if err := ValidateChangeDraft([]byte(doc)); err == nil {
		t.Fatal("employee suggestion was allowed to request publish")
	}
}

func TestValidateChangeDraftRejectsInvalidDateTimeFormat(t *testing.T) {
	doc := []byte(`{"change_id":"chg-1","enterprise_id":"ent-1","org_unit_id":"dept-rd","resource_type":"knowledge_entry","resource_id":"k-1","action":"update","requester_user_id":"u-1","origin":"direct_edit","permission_mode":"direct_edit","revision":1,"state":"draft","base_version":0,"proposed_content":{},"created_at":"not-a-time","updated_at":"also-not-a-time"}`)
	if err := ValidateChangeDraft(doc); err == nil {
		t.Fatal("invalid ChangeDraft date-time accepted")
	}
}
