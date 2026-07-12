package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

type NexusAuthorizer struct{ Client nexus.GovernanceClient }

func (a NexusAuthorizer) Authorize(ctx context.Context, actor Actor, org string, typ model.ResourceType, id, action string) error {
	if a.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") || actor.OrgVersion < 1 {
		return ErrForbidden
	}
	resourceType := "knowledge"
	prefix := "knowledge"
	if typ == model.ResourceWorkflow {
		resourceType = "workflow"
		prefix = "workflow"
	}
	mapped := prefix + "." + action
	switch action {
	case "edit":
		mapped = prefix + ".edit"
	case "submit":
		mapped = prefix + ".edit"
	case "decide":
		if actor.has("approve_high_risk") {
			mapped = prefix + ".approve_high_risk"
		} else {
			mapped = prefix + ".publish_low_risk"
		}
	case "publish":
		// Final publication moves the pointer of an already-approved change.
		// Low-risk direct-publish authority is consumed when the route is decided;
		// the publishing editor only needs resource edit authority here.
		mapped = prefix + ".edit"
	}
	req := nexus.BrowserAuthorizationRequest{OrgUnitID: org, OrgVersion: actor.OrgVersion, ResourceType: resourceType, ResourceID: id, Action: mapped}
	decision, err := authorizeNexus(ctx, a.Client, actor, req)
	if err != nil || decision.Decision != "allow" || decision.OrgVersion != actor.OrgVersion || !contains(decision.OrgUnitIDs, org) {
		return ErrForbidden
	}
	return nil
}

func (a NexusAuthorizer) AuthorizeDecision(ctx context.Context, actor Actor, rec Record, _ DecisionInput) error {
	if a.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") || actor.OrgVersion < 1 {
		return ErrForbidden
	}
	prefix := "knowledge"
	resourceType := "knowledge"
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		prefix, resourceType = "workflow", "workflow"
	}
	action := prefix + ".publish_low_risk"
	if rec.Route.Mode != model.ReviewSingleConfirmation {
		action = prefix + ".approve_high_risk"
	}
	req := nexus.BrowserAuthorizationRequest{OrgUnitID: rec.Draft.OrgUnitID, OrgVersion: actor.OrgVersion, ResourceType: resourceType, ResourceID: rec.Draft.ResourceID, Action: action}
	if rec.Route.Mode == model.ReviewAdminQueue {
		req.ReviewMode, req.Queue = string(rec.Route.Mode), rec.Route.Queue
	}
	decision, err := authorizeNexus(ctx, a.Client, actor, req)
	if err != nil || decision.Decision != "allow" || decision.OrgVersion != actor.OrgVersion || !contains(decision.OrgUnitIDs, rec.Draft.OrgUnitID) {
		return ErrForbidden
	}
	return nil
}

type NexusRouteResolver struct {
	Client nexus.GovernanceClient
	Now    func() time.Time
}

func (NexusRouteResolver) Authoritative() bool { return true }

func (r NexusRouteResolver) Resolve(ctx context.Context, actor Actor, rec Record, assessment model.RiskAssessment) (model.ReviewRoute, error) {
	if r.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") {
		return model.ReviewRoute{}, ErrForbidden
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	fields := changedFields(rec.Content)
	req := nexus.ApprovalResolveRequest{TicketID: actor.UpstreamTicketID, EnterpriseID: actor.EnterpriseID, ActorUserID: actor.UserID, IdempotencyKey: stableID("route", actor.EnterpriseID, rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision)), OrgVersion: actor.OrgVersion, OrgUnitID: rec.Draft.OrgUnitID, ResourceType: string(rec.Draft.ResourceType), ResourceID: rec.Draft.ResourceID, Action: string(rec.Draft.Action), ChangedFields: fields, ImpactedOrgUnitIDs: []string{rec.Draft.OrgUnitID}, PublishedBehaviorChange: rec.Draft.ResourceType == model.ResourceWorkflow, RequestedRisk: string(assessment.RiskLevel), FactsIssuedAt: now, FactsExpiresAt: now.Add(5 * time.Minute), FactsNonce: stableID("nonce", rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision))}
	var route nexus.ApprovalRoute
	var err error
	if actor.UpstreamAccessToken != "" {
		route, err = r.Client.ResolveApprovalRouteWithBearer(ctx, actor.UpstreamAccessToken, req)
	} else {
		route, err = r.Client.ResolveApprovalRoute(ctx, req)
	}
	if err != nil || route.AutoPublish {
		return model.ReviewRoute{}, ErrForbidden
	}
	risk := model.RiskLevel(route.RiskLevel)
	if risk != model.RiskLow {
		risk = model.RiskHigh
	}
	if assessment.RiskLevel == model.RiskHigh && risk != model.RiskHigh {
		return model.ReviewRoute{}, ErrForbidden
	}
	mode := model.ReviewMode(route.Mode)
	return model.ReviewRoute{ChangeID: rec.Draft.ChangeID, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID, RequesterUserID: rec.Draft.RequesterUserID, ReviewerUserID: route.ReviewerUserID, RiskLevel: risk, Mode: mode, State: model.RoutePending, OrgPath: route.OrgPath, Queue: route.Queue}, nil
}

type NexusAuditAppender struct{ Client nexus.GovernanceClient }

func (a NexusAuditAppender) Append(ctx context.Context, actor Actor, rec Record, key string) (string, error) {
	if a.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") {
		return "", ErrForbidden
	}
	action := nexus.AuditVisibilityRuleChanged
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		action = nexus.AuditWorkflowVersionPublished
	}
	prefix := "knowledge"
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		prefix = "workflow"
	}
	req := nexus.AppendAuditEvidenceRequest{IdempotencyKey: key, TicketID: actor.UpstreamTicketID, EnterpriseID: actor.EnterpriseID, Action: action, ResourceType: string(rec.Draft.ResourceType), ResourceID: rec.Draft.ResourceID, Details: map[string]any{"change_id": rec.Draft.ChangeID, "revision": rec.Draft.Revision, "org_unit_id": rec.Draft.OrgUnitID}, OrgVersion: actor.OrgVersion, OrgUnitID: rec.Draft.OrgUnitID, AuthorizedAction: prefix + ".edit"}
	var out nexus.AppendAuditEvidenceResponse
	var err error
	if actor.UpstreamAccessToken != "" {
		out, err = a.Client.AppendAuditEvidenceWithBearer(ctx, actor.UpstreamAccessToken, req)
	} else {
		out, err = a.Client.AppendAuditEvidence(ctx, req)
	}
	if err != nil || out.AuditRefID == "" {
		return "", fmt.Errorf("mandatory governance audit: %w", err)
	}
	return out.AuditRefID, nil
}

func (a NexusAuditAppender) AppendDecision(ctx context.Context, actor Actor, rec Record, in DecisionInput) error {
	if rec.Route.Mode != model.ReviewAdminQueue {
		return nil
	}
	if a.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") {
		return ErrForbidden
	}
	req := nexus.AppendAuditEvidenceRequest{
		IdempotencyKey: stableID("decision", actor.EnterpriseID, rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision), actor.UserID, in.Decision),
		TicketID:       actor.UpstreamTicketID,
		EnterpriseID:   actor.EnterpriseID,
		Action:         nexus.AuditGovernanceChangeDecided,
		ResourceType:   string(rec.Draft.ResourceType),
		ResourceID:     rec.Draft.ResourceID,
		Details: map[string]any{"change_id": rec.Draft.ChangeID, "revision": rec.Draft.Revision, "decision": in.Decision,
			"review_mode": string(rec.Route.Mode), "queue": rec.Route.Queue, "org_unit_id": rec.Draft.OrgUnitID},
		OrgVersion: actor.OrgVersion, OrgUnitID: rec.Draft.OrgUnitID, AuthorizedAction: func() string {
			if rec.Draft.ResourceType == model.ResourceWorkflow {
				return "workflow.approve_high_risk"
			}
			return "knowledge.approve_high_risk"
		}(), ReviewMode: string(rec.Route.Mode), Queue: rec.Route.Queue,
	}
	var out nexus.AppendAuditEvidenceResponse
	var err error
	if actor.UpstreamAccessToken != "" {
		out, err = a.Client.AppendAuditEvidenceWithBearer(ctx, actor.UpstreamAccessToken, req)
	} else {
		out, err = a.Client.AppendAuditEvidence(ctx, req)
	}
	if err != nil || out.AuditRefID == "" {
		return fmt.Errorf("mandatory governance decision audit: %w", err)
	}
	return nil
}
func authorizeNexus(ctx context.Context, client nexus.GovernanceClient, actor Actor, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	if actor.UpstreamAccessToken != "" {
		return client.AuthorizeBrowserOperation(ctx, actor.UpstreamAccessToken, req)
	}
	req.TicketID = actor.UpstreamTicketID
	return client.AuthorizeTicketOperation(ctx, actor.UpstreamTicketID, req)
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func changedFields(raw []byte) []string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
