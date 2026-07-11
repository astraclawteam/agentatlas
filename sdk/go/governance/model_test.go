package governance

import "testing"

func TestReviewRouteValidation(t *testing.T) {
	valid := ReviewRoute{ChangeID: "chg", ResourceType: ResourceWorkflow, ResourceID: "wf", RequesterUserID: "u1", ReviewerUserID: "u2", RiskLevel: RiskHigh, Mode: ReviewUpward, State: RoutePending, OrgPath: []string{"company"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	valid.ReviewerUserID = "u1"
	if err := valid.Validate(); err == nil {
		t.Fatal("self review accepted")
	}
	invalid := []ReviewRoute{
		{Mode: ReviewSingleConfirmation, RiskLevel: RiskLow, Queue: "admins"},
		{Mode: ReviewAdminQueue, RiskLevel: RiskHigh, Queue: "admins", ReviewerUserID: "u2", RequesterUserID: "u1"},
		{Mode: ReviewUpward, RiskLevel: RiskHigh, ReviewerUserID: "u2"},
	}
	for i, route := range invalid {
		if err := route.Validate(); err == nil {
			t.Fatalf("invalid route %d accepted", i)
		}
	}
}

func TestEmployeeSuggestionCannotPublish(t *testing.T) {
	draft := ChangeDraft{Origin: OriginEmployeeSuggestion, PermissionMode: PermissionSuggestionOnly, Action: ActionPublish}
	if err := draft.Validate(); err == nil {
		t.Fatal("employee suggestion publish accepted")
	}
}
