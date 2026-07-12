package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	governancemodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

// TestBrowserGovernance starts at the two security boundaries that previously
// did not exist: Atlas owns opaque browser sessions, and every maintenance
// publish consumes an immutable governed change.
func TestBrowserGovernance(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	sessions := browsersession.NewMemoryStore(clock)
	login, err := sessions.CreateLoginAttempt(ctx, browsersession.LoginAttemptInput{
		State: "state-1234567890123456", Nonce: "nonce-1234567890123456",
		PKCEVerifier: "verifier-1234567890123456789012345678901234567890123",
		ReturnTo:     "/changes/chg-1",
	})
	if err != nil || login.ReturnTo != "/changes/chg-1" {
		t.Fatalf("create login attempt: %+v %v", login, err)
	}
	if _, err := sessions.ConsumeLoginAttempt(ctx, login.State); err != nil {
		t.Fatalf("consume login attempt: %v", err)
	}
	if _, err := sessions.ConsumeLoginAttempt(ctx, login.State); !errors.Is(err, browsersession.ErrInvalidState) {
		t.Fatalf("state reused: %v", err)
	}

	actor := governance.Actor{EnterpriseID: "ent-1", UserID: "employee", OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest"}}
	store := governance.NewMemoryStore(clock)
	audits := &governance.MemoryAuditAppender{}
	pub := governance.NewMemoryPublisher()
	routes := governance.StaticRouteResolver{ReviewerUserID: "manager", OrgPath: []string{"team", "department"}}
	svc := governance.NewService(store, routes, audits, pub, clock)

	draft, err := svc.Suggest(ctx, actor, governance.SuggestionInput{
		OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow,
		ResourceID: "wf-1", Action: governancemodel.ActionUpdate,
		ProposedContent: json.RawMessage(`{"nodes":[{"id":"external"}]}`),
	})
	if err != nil || draft.PermissionMode != governancemodel.PermissionSuggestionOnly {
		t.Fatalf("employee suggestion: %+v %v", draft, err)
	}
	if _, err := svc.UpdateDraft(ctx, actor, draft.ChangeID, 1, json.RawMessage(`{"nodes":[]}`)); !errors.Is(err, governance.ErrForbidden) {
		t.Fatalf("suggestion-only actor updated draft: %v", err)
	}

	editor := governance.Actor{EnterpriseID: "ent-1", UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest", "workflow_edit"}}
	draft, err = svc.UpdateDraft(ctx, editor, draft.ChangeID, 1, json.RawMessage(`{"nodes":[{"id":"external-action"}]}`))
	if err != nil || draft.Revision != 2 {
		t.Fatalf("revisioned update: %+v %v", draft, err)
	}
	if _, err := svc.UpdateDraft(ctx, editor, draft.ChangeID, 1, json.RawMessage(`{"nodes":[]}`)); !errors.Is(err, governance.ErrConflict) {
		t.Fatalf("stale update did not conflict: %v", err)
	}
	assessment, err := svc.Assess(ctx, editor, draft.ChangeID)
	if err != nil || assessment.RiskLevel != governancemodel.RiskHigh {
		t.Fatalf("deterministic risk: %+v %v", assessment, err)
	}
	route, err := svc.Submit(ctx, editor, draft.ChangeID)
	if err != nil || route.ReviewerUserID != "manager" || route.Mode != governancemodel.ReviewUpward {
		t.Fatalf("upward route: %+v %v", route, err)
	}
	if err := svc.Decide(ctx, editor, draft.ChangeID, "e2e-self-decision-key-0001", governance.DecisionInput{Decision: "approve"}); !errors.Is(err, governance.ErrForbidden) {
		t.Fatalf("self review accepted: %v", err)
	}
	reviewer := governance.Actor{EnterpriseID: "ent-1", UserID: "manager", OrgUnitIDs: []string{"department"}, Permissions: []string{"approve_high_risk"}}
	if err := svc.Decide(ctx, reviewer, draft.ChangeID, "e2e-review-decision-key-0001", governance.DecisionInput{Decision: "approve"}); err != nil {
		t.Fatalf("upward decision: %v", err)
	}
	first, err := svc.Publish(ctx, editor, draft.ChangeID, "publish-key-1234567890")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	second, err := svc.Publish(ctx, editor, draft.ChangeID, "publish-key-1234567890")
	if err != nil || first != second || first.Version != 1 || audits.Count() != 1 || pub.Count() != 1 {
		t.Fatalf("idempotent publish first=%+v second=%+v err=%v audits=%d publishes=%d", first, second, err, audits.Count(), pub.Count())
	}
}
