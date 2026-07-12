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

type NexusAuthorizer struct{ Client nexus.BrowserBFFClient }

func (a NexusAuthorizer) Authorize(ctx context.Context, actor Actor, org string, typ model.ResourceType, id, action string) error {
	if a.Client == nil || actor.UpstreamAccessToken == "" || actor.OrgVersion < 1 {
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
		mapped = prefix + ".publish_low_risk"
	}
	decision, err := a.Client.AuthorizeBrowserOperation(ctx, actor.UpstreamAccessToken, nexus.BrowserAuthorizationRequest{OrgUnitID: org, OrgVersion: actor.OrgVersion, ResourceType: resourceType, ResourceID: id, Action: mapped})
	if err != nil || decision.Decision != "allow" || decision.OrgVersion != actor.OrgVersion || !contains(decision.OrgUnitIDs, org) {
		return ErrForbidden
	}
	return nil
}

type NexusRouteResolver struct {
	Client nexus.BrowserBFFClient
	Now    func() time.Time
}

func (NexusRouteResolver) Authoritative() bool { return true }

func (r NexusRouteResolver) Resolve(ctx context.Context, actor Actor, rec Record, assessment model.RiskAssessment) (model.ReviewRoute, error) {
	if r.Client == nil || actor.UpstreamAccessToken == "" {
		return model.ReviewRoute{}, ErrForbidden
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	fields := changedFields(rec.Content)
	route, err := r.Client.ResolveApprovalRouteWithBearer(ctx, actor.UpstreamAccessToken, nexus.ApprovalResolveRequest{EnterpriseID: actor.EnterpriseID, ActorUserID: actor.UserID, IdempotencyKey: stableID("route", actor.EnterpriseID, rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision)), OrgVersion: actor.OrgVersion, OrgUnitID: rec.Draft.OrgUnitID, ResourceType: string(rec.Draft.ResourceType), ResourceID: rec.Draft.ResourceID, Action: string(rec.Draft.Action), ChangedFields: fields, ImpactedOrgUnitIDs: []string{rec.Draft.OrgUnitID}, PublishedBehaviorChange: rec.Draft.ResourceType == model.ResourceWorkflow, RequestedRisk: string(assessment.RiskLevel), FactsIssuedAt: now, FactsExpiresAt: now.Add(5 * time.Minute), FactsNonce: stableID("nonce", rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision))})
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

type NexusAuditAppender struct{ Client nexus.BrowserBFFClient }

func (a NexusAuditAppender) Append(ctx context.Context, actor Actor, rec Record, key string) (string, error) {
	if a.Client == nil || actor.UpstreamAccessToken == "" {
		return "", ErrForbidden
	}
	action := nexus.AuditVisibilityRuleChanged
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		action = nexus.AuditWorkflowVersionPublished
	}
	out, err := a.Client.AppendAuditEvidenceWithBearer(ctx, actor.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: key, EnterpriseID: actor.EnterpriseID, Action: action, ResourceType: string(rec.Draft.ResourceType), ResourceID: rec.Draft.ResourceID, Details: map[string]any{"change_id": rec.Draft.ChangeID, "revision": rec.Draft.Revision, "org_unit_id": rec.Draft.OrgUnitID}})
	if err != nil || out.AuditRefID == "" {
		return "", fmt.Errorf("mandatory governance audit: %w", err)
	}
	return out.AuditRefID, nil
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
