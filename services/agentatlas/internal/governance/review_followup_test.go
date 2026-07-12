package governance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
)

func TestWorkflowUsesWorkflowEditAndApprovedHighRiskPublishDoesNotRequireLowRiskPrivilege(t *testing.T) {
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC) }
	svc := NewService(NewMemoryStore(clock), StaticRouteResolver{ReviewerUserID: "reviewer", OrgPath: []string{"team", "division"}}, &MemoryAuditAppender{}, NewMemoryPublisher(), clock)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf-1", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatalf("workflow_edit create: %v", err)
	}
	if _, err := svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatalf("workflow_edit submit: %v", err)
	}
	reviewer := Actor{EnterpriseID: "ent", UserID: "reviewer", OrgUnitIDs: []string{"division"}, Permissions: []string{"approve_high_risk"}}
	if err := svc.Decide(ctx, reviewer, draft.ChangeID, DecisionInput{Decision: "approve"}); err != nil {
		t.Fatalf("separate reviewer: %v", err)
	}
	if _, err := svc.Publish(ctx, editor, draft.ChangeID, "approved-high-risk-publish"); err != nil {
		t.Fatalf("approved high-risk editor publish: %v", err)
	}
}

func TestGenericEditCannotEditWorkflow(t *testing.T) {
	svc := NewService(NewMemoryStore(time.Now), StaticRouteResolver{}, &MemoryAuditAppender{}, nil, time.Now)
	_, err := svc.CreateDraft(context.Background(), Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"edit"}}, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionUpdate, ProposedContent: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("generic edit unexpectedly edited workflow: %v", err)
	}
}

func TestEnterpriseKnowledgeAdminQueueCanBeDecidedByDifferentHighRiskApprover(t *testing.T) {
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC) }
	svc := NewService(NewMemoryStore(clock), adminQueueResolver{}, &MemoryAuditAppender{}, nil, clock)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatal(err)
	}
	admin := Actor{EnterpriseID: "ent", UserID: "knowledge-admin", OrgUnitIDs: []string{"team"}, Permissions: []string{"approve_high_risk"}}
	if err := svc.Decide(ctx, admin, draft.ChangeID, DecisionInput{Decision: "approve", Comment: "reviewed"}); err != nil {
		t.Fatalf("admin queue decision: %v", err)
	}
	if err := svc.Decide(ctx, editor, draft.ChangeID, DecisionInput{Decision: "approve"}); !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrForbidden) {
		t.Fatalf("requester decided admin queue: %v", err)
	}
}

func TestDecisionAuditFailureLeavesReviewPending(t *testing.T) {
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC) }
	store := NewMemoryStore(clock)
	auditor := &failingDecisionAuditor{}
	svc := NewService(store, adminQueueResolver{}, auditor, nil, clock)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatal(err)
	}
	admin := Actor{EnterpriseID: "ent", UserID: "admin", Permissions: []string{"approve_high_risk"}}
	if err := svc.Decide(ctx, admin, draft.ChangeID, DecisionInput{Decision: "approve"}); err == nil {
		t.Fatal("decision succeeded without mandatory audit")
	}
	rec, err := store.Get(ctx, "ent", draft.ChangeID)
	if err != nil || rec.Draft.State != model.ChangeSubmitted || rec.Route.State != model.RoutePending {
		t.Fatalf("state moved after audit failure: %+v err=%v", rec, err)
	}
}

type adminQueueResolver struct{}

func (adminQueueResolver) Resolve(_ context.Context, _ Actor, rec Record, assessment model.RiskAssessment) (model.ReviewRoute, error) {
	return model.ReviewRoute{ChangeID: rec.Draft.ChangeID, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID, RequesterUserID: rec.Draft.RequesterUserID, RiskLevel: assessment.RiskLevel, Mode: model.ReviewAdminQueue, State: model.RoutePending, OrgPath: []string{}, Queue: "enterprise_knowledge_admin"}, nil
}

type failingDecisionAuditor struct{ MemoryAuditAppender }

func (*failingDecisionAuditor) AppendDecision(context.Context, Actor, Record, DecisionInput) error {
	return errors.New("audit unavailable")
}
