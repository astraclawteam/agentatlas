package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

func TestNexusRouteCannotLowerLocalHighRisk(t *testing.T) {
	client := fakeBrowserNexus{route: nexus.ApprovalRoute{Mode: string(model.ReviewSingleConfirmation), RiskLevel: "low", RequesterUserID: "editor", OrgPath: []string{}, AutoPublish: false}}
	resolver := NexusRouteResolver{Client: client, Now: func() time.Time { return time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC) }}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamAccessToken: "upstream-access-token", OrgVersion: 7}
	if _, err := resolver.Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh, RiskReasons: []string{"workflow"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("high risk lowered: %v", err)
	}
}

func TestApprovedPublishAuthorizesWorkflowEditNotLowRiskPublish(t *testing.T) {
	client := &recordingBrowserNexus{}
	authorizer := NexusAuthorizer{Client: client}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamAccessToken: "token", OrgVersion: 7}
	if err := authorizer.Authorize(context.Background(), actor, "team", model.ResourceWorkflow, "wf", "publish"); err != nil {
		t.Fatalf("authorize publish: %v", err)
	}
	if client.request.Action != "workflow.edit" {
		t.Fatalf("final publish action=%q, want workflow.edit", client.request.Action)
	}
}

func TestCaseTicketActorUsesServiceCredentialGovernanceAdapters(t *testing.T) {
	client := &recordingBrowserNexus{route: nexus.ApprovalRoute{Mode: string(model.ReviewUpward), RiskLevel: "high", RequesterUserID: "editor", ReviewerUserID: "reviewer", OrgPath: []string{"team", "division"}}}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamTicketID: "case-ticket", OrgVersion: 7, OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	if err := (NexusAuthorizer{Client: client}).Authorize(context.Background(), actor, "team", model.ResourceWorkflow, "wf", "edit"); err != nil {
		t.Fatalf("ticket authorize: %v", err)
	}
	if client.ticketCalls != 1 || client.bearerCalls != 0 || client.request.TicketID != "case-ticket" {
		t.Fatalf("wrong credential path: ticket=%d bearer=%d req=%+v", client.ticketCalls, client.bearerCalls, client.request)
	}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	if _, err := (NexusRouteResolver{Client: client}).Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh}); err != nil {
		t.Fatalf("ticket approval: %v", err)
	}
	if _, err := (NexusAuditAppender{Client: client}).Append(context.Background(), actor, rec, "ticket-audit-key-123"); err != nil {
		t.Fatalf("ticket audit: %v", err)
	}
	if client.ticketRouteCalls != 1 || client.ticketAuditCalls != 1 {
		t.Fatalf("service route/audit calls=%d/%d", client.ticketRouteCalls, client.ticketAuditCalls)
	}
}

func TestAdminQueueDecisionBindsModeQueueAndAuditsTransition(t *testing.T) {
	client := &recordingBrowserNexus{}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg-admin", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", RequesterUserID: "editor", Revision: 3}, Route: model.ReviewRoute{Mode: model.ReviewAdminQueue, Queue: "enterprise_knowledge_admin", RiskLevel: model.RiskHigh}}
	actor := Actor{EnterpriseID: "ent", UserID: "admin", UpstreamTicketID: "admin-ticket", OrgVersion: 8, OrgUnitIDs: []string{"team"}, Permissions: []string{"approve_high_risk"}}
	decision := DecisionInput{Decision: "approve", Comment: "reviewed"}
	if err := (NexusAuthorizer{Client: client}).AuthorizeDecision(context.Background(), actor, rec, decision); err != nil {
		t.Fatalf("authorize admin decision: %v", err)
	}
	if client.request.Action != "workflow.approve_high_risk" || client.request.ReviewMode != string(model.ReviewAdminQueue) || client.request.Queue != "enterprise_knowledge_admin" {
		t.Fatalf("unbound decision request: %+v", client.request)
	}
	if err := (NexusAuditAppender{Client: client}).AppendDecision(context.Background(), actor, rec, decision); err != nil {
		t.Fatalf("decision audit: %v", err)
	}
	if client.auditRequest.Action != nexus.AuditGovernanceChangeDecided || client.auditRequest.OrgVersion != 8 || client.auditRequest.OrgUnitID != "team" || client.auditRequest.AuthorizedAction != "workflow.approve_high_risk" || client.auditRequest.ReviewMode != string(model.ReviewAdminQueue) || client.auditRequest.Queue != "enterprise_knowledge_admin" || client.auditRequest.Details["review_mode"] != string(model.ReviewAdminQueue) || client.auditRequest.Details["queue"] != "enterprise_knowledge_admin" {
		t.Fatalf("unbound decision audit: %+v", client.auditRequest)
	}
}

type fakeBrowserNexus struct{ route nexus.ApprovalRoute }

func (f fakeBrowserNexus) AuthorizeBrowserOperation(context.Context, string, nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{}, nil
}
func (f fakeBrowserNexus) AuthorizeTicketOperation(context.Context, string, nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{}, nil
}
func (f fakeBrowserNexus) ResolveApprovalRouteWithBearer(context.Context, string, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return f.route, nil
}
func (f fakeBrowserNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}
func (f fakeBrowserNexus) ResolveApprovalRoute(context.Context, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return f.route, nil
}
func (f fakeBrowserNexus) AppendAuditEvidence(context.Context, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}

type recordingBrowserNexus struct {
	request                            nexus.BrowserAuthorizationRequest
	auditRequest                       nexus.AppendAuditEvidenceRequest
	route                              nexus.ApprovalRoute
	ticketCalls, bearerCalls           int
	ticketRouteCalls, ticketAuditCalls int
}

func (f *recordingBrowserNexus) AuthorizeBrowserOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	f.request = req
	f.bearerCalls++
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (f *recordingBrowserNexus) AuthorizeTicketOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	f.request = req
	f.ticketCalls++
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (*recordingBrowserNexus) ResolveApprovalRouteWithBearer(context.Context, string, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return nexus.ApprovalRoute{}, nil
}
func (*recordingBrowserNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}
func (f *recordingBrowserNexus) ResolveApprovalRoute(context.Context, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	f.ticketRouteCalls++
	return f.route, nil
}
func (f *recordingBrowserNexus) AppendAuditEvidence(_ context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	f.auditRequest = req
	f.ticketAuditCalls++
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-ref"}, nil
}
