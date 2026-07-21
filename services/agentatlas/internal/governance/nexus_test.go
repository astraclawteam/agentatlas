package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// TestNexusRouteRiskIsLocalAndTransmitNamesNoReviewer replaces
// TestNexusRouteCannotLowerLocalHighRisk. That test guarded against AgentNexus
// downgrading a locally-assessed risk. AgentNexus no longer returns a risk to
// downgrade, so the guard became structural; what needs asserting now is that
// the local assessment is what survives, and that a transmitted plan names no
// reviewer.
func TestNexusRouteRiskIsLocalAndTransmitNamesNoReviewer(t *testing.T) {
	transmitter := &fakeApprovalTransmitter{}
	resolver := NexusRouteResolver{Client: fakeBrowserNexus{}, Transmit: transmitter, Authority: "oa.example", Now: func() time.Time { return time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC) }}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamAccessToken: "upstream-access-token", OrgVersion: 7}

	route, err := resolver.Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh, RiskReasons: []string{"workflow"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.RiskLevel != model.RiskHigh {
		t.Fatalf("local risk did not survive: %+v", route)
	}
	// Queued with the authority, not assigned upward: ReviewUpward would
	// require a reviewer, and naming one at transmit time would be a fiction.
	if route.Mode != model.ReviewAdminQueue || route.Queue != "oa.example" {
		t.Fatalf("transmitted route is not queued with the authority: %+v", route)
	}
	if route.ReviewerUserID != "" || len(route.OrgPath) != 0 {
		t.Fatalf("a transmitted plan must name no reviewer yet: %+v", route)
	}
	// Nothing was transmitted: "chg" is not a wc_* WorkCase handle, and the
	// frozen ApprovalRequest rejects anything else client-side. Transmission is
	// dormant until C1 makes governed changes WorkCase-backed; the route is
	// still resolved, so governance keeps working locally in the meantime.
	if len(transmitter.sent) != 0 {
		t.Fatalf("transmitted a plan for a change with no WorkCase: %+v", transmitter.sent)
	}
}

// The transmit branch is still real, and this is what proves it: give the
// change a WorkCase handle and the plan goes out. Without this the dormant
// path above would be the only one covered, and the transmission code would
// rot untested until C1 lands.
func TestNexusRouteTransmitsOnceTheChangeIsWorkCaseBacked(t *testing.T) {
	transmitter := &fakeApprovalTransmitter{}
	resolver := NexusRouteResolver{Client: fakeBrowserNexus{}, Transmit: transmitter, Authority: "oa.example", Now: func() time.Time { return time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC) }}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "wc_0123456789abcdef", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamAccessToken: "upstream-access-token", OrgVersion: 7}

	route, err := resolver.Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(transmitter.sent) != 1 || transmitter.sent[0].Plan.Authority != "oa.example" {
		t.Fatalf("plan not transmitted to the configured authority: %+v", transmitter.sent)
	}
	if transmitter.sent[0].BusinessContextRef != "wc_0123456789abcdef" {
		t.Fatalf("business context must be the WorkCase handle: %+v", transmitter.sent[0])
	}
	if route.Mode != model.ReviewAdminQueue || route.ReviewerUserID != "" {
		t.Fatalf("a transmitted plan must name no reviewer yet: %+v", route)
	}
}

// TestNexusRouteRefusesWithoutAnApprovalAuthority: an unset authority is a
// deployment gap, and AgentNexus will not stand in for one.
func TestNexusRouteRefusesWithoutAnApprovalAuthority(t *testing.T) {
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamAccessToken: "upstream-access-token", OrgVersion: 7}
	resolver := NexusRouteResolver{Client: fakeBrowserNexus{}, Transmit: &fakeApprovalTransmitter{}}
	if _, err := resolver.Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing authority accepted: %v", err)
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
	client := &recordingBrowserNexus{}
	actor := Actor{EnterpriseID: "ent", UserID: "editor", UpstreamTicketID: "case-ticket", OrgVersion: 7, OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	if err := (NexusAuthorizer{Client: client}).Authorize(context.Background(), actor, "team", model.ResourceWorkflow, "wf", "edit"); err != nil {
		t.Fatalf("ticket authorize: %v", err)
	}
	if client.ticketCalls != 1 || client.bearerCalls != 0 || client.request.TicketID != "case-ticket" {
		t.Fatalf("wrong credential path: ticket=%d bearer=%d req=%+v", client.ticketCalls, client.bearerCalls, client.request)
	}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg", EnterpriseID: "ent", OrgUnitID: "team", ResourceType: model.ResourceWorkflow, ResourceID: "wf", Action: model.ActionPublish, RequesterUserID: "editor", Revision: 1}, Content: []byte(`{"nodes":[]}`)}
	transmitter := &fakeApprovalTransmitter{}
	if _, err := (NexusRouteResolver{Client: client, Transmit: transmitter, Authority: "oa.example"}).Resolve(context.Background(), actor, rec, model.RiskAssessment{RiskLevel: model.RiskHigh}); err != nil {
		t.Fatalf("ticket approval: %v", err)
	}
	if _, err := (NexusAuditAppender{Client: client}).Append(context.Background(), actor, rec, "ticket-audit-key-123"); err != nil {
		t.Fatalf("ticket audit: %v", err)
	}
	// The route no longer travels through the nexus client. Nothing is
	// transmitted either: "chg" is not a WorkCase handle, so transmission stays
	// dormant until C1. What this test is actually about is unchanged -- the
	// audit still goes through the service-credential path.
	if len(transmitter.sent) != 0 || client.ticketAuditCalls != 1 {
		t.Fatalf("transmit/audit calls=%d/%d", len(transmitter.sent), client.ticketAuditCalls)
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
	if _, err := (NexusAuditAppender{Client: client}).AppendDecision(context.Background(), actor, rec, decision, "decision-operation-key-001"); err != nil {
		t.Fatalf("decision audit: %v", err)
	}
	if client.auditRequest.Action != nexus.AuditGovernanceChangeDecided || client.auditRequest.OrgVersion != 8 || client.auditRequest.OrgUnitID != "team" || client.auditRequest.AuthorizedAction != "workflow.approve_high_risk" || client.auditRequest.ReviewMode != string(model.ReviewAdminQueue) || client.auditRequest.Queue != "enterprise_knowledge_admin" || client.auditRequest.Details["review_mode"] != string(model.ReviewAdminQueue) || client.auditRequest.Details["queue"] != "enterprise_knowledge_admin" {
		t.Fatalf("unbound decision audit: %+v", client.auditRequest)
	}
}

func TestAdminQueueDecisionAcceptsAuthoritativeInheritedRootScope(t *testing.T) {
	client := &recordingBrowserNexus{decisionOrgUnitIDs: []string{"company_root"}}
	rec := Record{Draft: model.ChangeDraft{ChangeID: "chg-admin", EnterpriseID: "ent", OrgUnitID: "team_research", ResourceType: model.ResourceWorkflow, ResourceID: "wf", RequesterUserID: "editor", Revision: 1}, Route: model.ReviewRoute{Mode: model.ReviewAdminQueue, Queue: "enterprise_knowledge_admin", RiskLevel: model.RiskHigh}}
	actor := Actor{EnterpriseID: "ent", UserID: "root-admin", UpstreamAccessToken: "browser-token", OrgVersion: 8, OrgUnitIDs: []string{"company_root"}, Permissions: []string{"approve_high_risk"}}
	if err := (NexusAuthorizer{Client: client}).AuthorizeDecision(context.Background(), actor, rec, DecisionInput{Decision: "approve"}); err != nil {
		t.Fatalf("authoritative inherited root decision rejected: %v", err)
	}
}

type fakeBrowserNexus struct{}

func (f fakeBrowserNexus) AuthorizeBrowserOperation(context.Context, string, nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{}, nil
}
func (f fakeBrowserNexus) AuthorizeTicketOperation(context.Context, string, nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{}, nil
}
func (f fakeBrowserNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}
func (f fakeBrowserNexus) AppendAuditEvidence(context.Context, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}

type recordingBrowserNexus struct {
	request                  nexus.BrowserAuthorizationRequest
	auditRequest             nexus.AppendAuditEvidenceRequest
	ticketCalls, bearerCalls int
	ticketAuditCalls         int
	decisionOrgUnitIDs       []string
}

func (f *recordingBrowserNexus) AuthorizeBrowserOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	f.request = req
	f.bearerCalls++
	orgs := f.decisionOrgUnitIDs
	if len(orgs) == 0 {
		orgs = []string{req.OrgUnitID}
	}
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: orgs}, nil
}
func (f *recordingBrowserNexus) AuthorizeTicketOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	f.request = req
	f.ticketCalls++
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (*recordingBrowserNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}
func (f *recordingBrowserNexus) AppendAuditEvidence(_ context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	f.auditRequest = req
	f.ticketAuditCalls++
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-ref"}, nil
}
