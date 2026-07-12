package governance

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
	if err := svc.Decide(ctx, reviewer, draft.ChangeID, "review-decision-key-0001", DecisionInput{Decision: "approve"}); err != nil {
		t.Fatalf("separate reviewer: %v", err)
	}
	if _, err := svc.Publish(ctx, editor, draft.ChangeID, "approved-high-risk-publish"); err != nil {
		t.Fatalf("approved high-risk editor publish: %v", err)
	}
}

func TestCompetingDecisionsReserveBeforeAudit(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(time.Now)
	auditor := &countingDecisionAuditor{}
	svc := NewService(store, adminQueueResolver{}, auditor, nil, time.Now)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf-race", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatal(err)
	}
	admin := Actor{EnterpriseID: "ent", UserID: "admin", Permissions: []string{"approve_high_risk"}}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, decision := range []string{"approve", "reject"} {
		go func(i int, decision string) {
			<-start
			results <- svc.Decide(ctx, admin, draft.ChangeID, []string{"concurrent-decision-key-approve", "concurrent-decision-key-reject"}[i], DecisionInput{Decision: decision})
		}(i, decision)
	}
	close(start)
	err1, err2 := <-results, <-results
	if (err1 == nil) == (err2 == nil) || (err1 != nil && !errors.Is(err1, ErrConflict)) || (err2 != nil && !errors.Is(err2, ErrConflict)) {
		t.Fatalf("results = %v, %v; want one success and one conflict", err1, err2)
	}
	if auditor.Count() != 1 {
		t.Fatalf("remote decision audits = %d, want 1", auditor.Count())
	}
}

func TestDecisionRetryAfterLostResponseDoesNotRepeatAudit(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryStore(time.Now)
	store := &lostDecisionResponseStore{Store: inner}
	auditor := &countingDecisionAuditor{}
	svc := NewService(store, adminQueueResolver{}, auditor, nil, time.Now)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf-retry", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatal(err)
	}
	admin := Actor{EnterpriseID: "ent", UserID: "admin", Permissions: []string{"approve_high_risk"}}
	key := "lost-response-decision-key-001"
	input := DecisionInput{Decision: "approve", Comment: "reviewed"}
	if err = svc.Decide(ctx, admin, draft.ChangeID, key, input); err == nil {
		t.Fatal("first response was not lost")
	}
	if err = svc.Decide(ctx, admin, draft.ChangeID, key, input); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if auditor.Count() != 1 {
		t.Fatalf("remote decision audits = %d, want 1", auditor.Count())
	}
	if err = svc.Decide(ctx, admin, draft.ChangeID, key, DecisionInput{Decision: "reject", Comment: "reviewed"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key different payload = %v, want conflict", err)
	}
	if auditor.Count() != 1 {
		t.Fatalf("audit repeated after payload conflict: %d", auditor.Count())
	}
}

func TestStaleSubmitCannotResurrectFinalDecision(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(time.Now)
	svc := NewService(store, adminQueueResolver{}, &countingDecisionAuditor{}, nil, time.Now)
	editor := Actor{EnterpriseID: "ent", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, SuggestionInput{OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf-stale-submit", Action: model.ActionPublish, ProposedContent: json.RawMessage(`{"nodes":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.Get(ctx, "ent", draft.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	stale.Draft.State = model.ChangeSubmitted
	stale.Assessment = model.RiskAssessment{RiskLevel: model.RiskHigh, RiskReasons: []string{"workflow"}}
	stale.Route, err = (adminQueueResolver{}).Resolve(ctx, editor, stale, stale.Assessment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, draft.ChangeID); err != nil {
		t.Fatal(err)
	}
	admin := Actor{EnterpriseID: "ent", UserID: "admin", Permissions: []string{"approve_high_risk"}}
	if err = svc.Decide(ctx, admin, draft.ChangeID, "stale-submit-decision-key-01", DecisionInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveReview(ctx, "ent", stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale submit = %v, want conflict", err)
	}
	current, err := store.Get(ctx, "ent", draft.ChangeID)
	if err != nil || current.Draft.State != model.ChangeApproved || current.Route.State != model.RouteApproved {
		t.Fatalf("final decision resurrected: %+v err=%v", current, err)
	}
}

func TestDecisionPayloadHashBindsAuthorizationContext(t *testing.T) {
	actor := Actor{EnterpriseID: "ent", UserID: "reviewer", OrgVersion: 9}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", Revision: 2, OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish}, Route: model.ReviewRoute{Mode: model.ReviewAdminQueue, Queue: "knowledge-admin", OrgPath: []string{"team", "company"}}}
	input := DecisionInput{Decision: "approve", Comment: "checked"}
	base := decisionPayloadHash(actor, rec, input)
	mutations := []Record{rec, rec, rec, rec, rec}
	mutations[0].Draft.OrgUnitID = "other-team"
	mutations[1].Draft.ResourceID = "other-wf"
	mutations[2].Draft.Action = model.ActionDisable
	mutations[3].Route.Mode = model.ReviewUpward
	mutations[4].Route.Queue = "other-queue"
	for i, mutated := range mutations {
		if got := decisionPayloadHash(actor, mutated, input); got == base {
			t.Errorf("mutation %d did not change decision hash", i)
		}
	}
}

type countingDecisionAuditor struct {
	MemoryAuditAppender
	mu   sync.Mutex
	refs map[string]string
}

func (a *countingDecisionAuditor) AppendDecision(_ context.Context, actor Actor, _ Record, _ DecisionInput, key string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refs == nil {
		a.refs = map[string]string{}
	}
	if ref := a.refs[key]; ref != "" {
		return ref, nil
	}
	ref := stableID("decision_audit", actor.EnterpriseID, key)
	a.refs[key] = ref
	return ref, nil
}
func (a *countingDecisionAuditor) Count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.refs) }

type lostDecisionResponseStore struct {
	Store
	mu   sync.Mutex
	lost bool
}

func (s *lostDecisionResponseStore) FinalizeDecision(ctx context.Context, ent, key string, actor Actor, rec Record, in DecisionInput, auditRef string) error {
	if err := s.Store.FinalizeDecision(ctx, ent, key, actor, rec, in, auditRef); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lost {
		s.lost = true
		return errors.New("response lost after commit")
	}
	return nil
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
	if err := svc.Decide(ctx, admin, draft.ChangeID, "review-decision-key-0002", DecisionInput{Decision: "approve", Comment: "reviewed"}); err != nil {
		t.Fatalf("admin queue decision: %v", err)
	}
	if err := svc.Decide(ctx, editor, draft.ChangeID, "review-decision-key-0003", DecisionInput{Decision: "approve"}); !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) {
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
	if err := svc.Decide(ctx, admin, draft.ChangeID, "review-decision-key-0004", DecisionInput{Decision: "approve"}); err == nil {
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

func (*failingDecisionAuditor) AppendDecision(context.Context, Actor, Record, DecisionInput, string) (string, error) {
	return "", errors.New("audit unavailable")
}
