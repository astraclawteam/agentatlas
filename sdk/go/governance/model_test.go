package governance

import (
	"strings"
	"testing"
	"time"
)

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

func validChangeDraft() ChangeDraft {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	return ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "dept", ResourceType: ResourceKnowledgeEntry, ResourceID: "k1", Action: ActionUpdate, RequesterUserID: "u1", Origin: OriginDirectEdit, PermissionMode: PermissionDirectEdit, Revision: 1, State: ChangeDraftState, ProposedContent: map[string]any{}, CreatedAt: now, UpdatedAt: now}
}

func TestChangeDraftValidationMatrix(t *testing.T) {
	if err := validChangeDraft().Validate(); err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}
	cases := map[string]func(*ChangeDraft){
		"missing id":     func(d *ChangeDraft) { d.ChangeID = "" },
		"bad resource":   func(d *ChangeDraft) { d.ResourceType = "unknown" },
		"bad action":     func(d *ChangeDraft) { d.Action = "unknown" },
		"bad origin":     func(d *ChangeDraft) { d.Origin = "unknown" },
		"bad permission": func(d *ChangeDraft) { d.PermissionMode = "unknown" },
		"bad state":      func(d *ChangeDraft) { d.State = "unknown" },
		"zero revision":  func(d *ChangeDraft) { d.Revision = 0 },
		"negative base":  func(d *ChangeDraft) { d.BaseVersion = -1 },
		"missing time":   func(d *ChangeDraft) { d.CreatedAt = time.Time{} },
		"oversize id":    func(d *ChangeDraft) { d.ResourceID = strings.Repeat("x", 129) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validChangeDraft()
			mutate(&d)
			if err := d.Validate(); err == nil {
				t.Fatal("invalid draft accepted")
			}
		})
	}
}

func TestRiskAssessmentValidation(t *testing.T) {
	if err := (RiskAssessment{RiskLevel: RiskHigh, RiskReasons: []string{"workflow changed"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, risk := range []RiskAssessment{{RiskLevel: "medium"}, {RiskLevel: RiskLow, RiskReasons: nil}, {RiskLevel: RiskLow, RiskReasons: []string{strings.Repeat("x", 257)}}} {
		if err := risk.Validate(); err == nil {
			t.Fatalf("invalid risk accepted: %+v", risk)
		}
	}
}

func TestValidationCountsUnicodeCodePointsLikeJSONSchema(t *testing.T) {
	draft := validChangeDraft()
	draft.ResourceID = strings.Repeat("知", 128)
	if err := draft.Validate(); err != nil {
		t.Fatalf("128 Unicode characters must satisfy maxLength 128: %v", err)
	}
	draft.ResourceID += "识"
	if err := draft.Validate(); err == nil {
		t.Fatal("129 Unicode characters must exceed maxLength 128")
	}

	risk := RiskAssessment{RiskLevel: RiskHigh, RiskReasons: []string{strings.Repeat("风", 256)}}
	if err := risk.Validate(); err != nil {
		t.Fatalf("256 Unicode characters must satisfy maxLength 256: %v", err)
	}
	risk.RiskReasons[0] += "险"
	if err := risk.Validate(); err == nil {
		t.Fatal("257 Unicode characters must exceed maxLength 256")
	}
}

func TestReviewRouteRejectsMissingBindingsAndInvalidEnums(t *testing.T) {
	base := ReviewRoute{ChangeID: "chg", ResourceType: ResourceWorkflow, ResourceID: "wf", RequesterUserID: "u1", RiskLevel: RiskLow, Mode: ReviewSingleConfirmation, State: RoutePending, OrgPath: []string{}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []ReviewRoute{
		{ResourceType: ResourceWorkflow, ResourceID: "wf", RequesterUserID: "u1", RiskLevel: RiskLow, Mode: ReviewSingleConfirmation, State: RoutePending},
		{ChangeID: "chg", ResourceType: "unknown", ResourceID: "wf", RequesterUserID: "u1", RiskLevel: RiskLow, Mode: ReviewSingleConfirmation, State: RoutePending},
		{ChangeID: "chg", ResourceType: ResourceWorkflow, ResourceID: "wf", RequesterUserID: "u1", RiskLevel: RiskLow, Mode: ReviewSingleConfirmation, State: "unknown"},
	} {
		if err := route.Validate(); err == nil {
			t.Fatalf("invalid route accepted: %+v", route)
		}
	}
}
