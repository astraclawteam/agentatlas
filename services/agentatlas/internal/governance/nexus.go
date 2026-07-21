package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
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
	// AgentNexus evaluates this exact target and returns the authoritative grant
	// scopes that justified its decision. An inherited root grant legitimately
	// names the root rather than echoing the requested child organization.
	if err != nil || decision.Decision != "allow" || decision.OrgVersion != actor.OrgVersion || len(decision.OrgUnitIDs) == 0 {
		return ErrForbidden
	}
	return nil
}

type NexusRouteResolver struct {
	Client nexus.GovernanceClient
	Now    func() time.Time
	// Transmit delivers the authored plan to the approval authority.
	Transmit ApprovalTransmitter
	// Authority names the deployment's approval authority - the customer's
	// OA/BPM system. AgentNexus transmits to it and never substitutes for it.
	Authority string
}

func (NexusRouteResolver) Authoritative() bool { return true }

// hexDigest binds the exact change under approval.
func hexDigest(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func (r NexusRouteResolver) Resolve(ctx context.Context, actor Actor, rec Record, assessment model.RiskAssessment) (model.ReviewRoute, error) {
	if r.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") {
		return model.ReviewRoute{}, ErrForbidden
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if r.Transmit == nil || r.Authority == "" {
		// An unset authority is a deployment gap, not a default. AgentNexus
		// will not stand in for one.
		return model.ReviewRoute{}, ErrForbidden
	}
	// Transmission requires a wc_* WorkCase handle as the business context, and
	// a governed change has none until C1 builds the WorkCase orchestrator.
	// Until then the route is resolved locally and nothing is transmitted:
	// sending the change id would be rejected by Validate() on every call, so
	// attempting it could only ever turn a resolvable route into ErrForbidden.
	businessContextRef, transmissible := WorkCaseContextFor(rec.Draft.ChangeID)
	if !transmissible {
		localRisk := assessment.RiskLevel
		if localRisk != model.RiskLow {
			localRisk = model.RiskHigh
		}
		return model.ReviewRoute{
			ChangeID: rec.Draft.ChangeID, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID,
			RequesterUserID: rec.Draft.RequesterUserID, ReviewerUserID: "", RiskLevel: localRisk,
			Mode: model.ReviewAdminQueue, Queue: r.Authority, State: model.RoutePending, OrgPath: []string{},
		}, nil
	}
	fields := changedFields(rec.Content)
	// The plan hash binds the exact change under approval, so an authority
	// cannot be handed one plan and later shown another.
	planHash := "sha256:" + hexDigest(actor.EnterpriseID, rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision), string(rec.Draft.Action), strings.Join(fields, ","))
	req := nexusruntime.ApprovalRequest{
		RequestID:          stableID("aprq", actor.EnterpriseID, rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision)),
		BusinessContextRef: businessContextRef,
		Capability:         string(rec.Draft.ResourceType) + "." + string(rec.Draft.Action),
		ParameterHash:      planHash,
		Purpose:            "governed_change_approval",
		Plan: nexusruntime.ApprovalPlanRef{
			PlanRef:   ApprovalPlanRefFor(actor.EnterpriseID, rec.Draft.ChangeID, uint64(rec.Draft.Revision)),
			PlanHash:  planHash,
			Authority: r.Authority,
		},
		ExpiresAt: now.Add(approvalPlanTTL),
	}
	var err error
	if actor.UpstreamAccessToken != "" {
		_, err = r.Transmit.TransmitApprovalPlanWithBearer(ctx, actor.UpstreamAccessToken, req)
	} else {
		_, err = r.Transmit.TransmitApprovalPlan(ctx, req)
	}
	if err != nil {
		return model.ReviewRoute{}, ErrForbidden
	}
	// The risk verdict is now purely local. The retired surface returned a risk
	// level that this code cross-checked against its own assessment; with
	// AgentNexus out of the risk business there is no second opinion, and the
	// local assessment stands alone.
	risk := assessment.RiskLevel
	if risk != model.RiskLow {
		risk = model.RiskHigh
	}
	// A transmitted change is queued with an external authority, and that is
	// exactly what ReviewAdminQueue models: a queue, and deliberately NO named
	// reviewer. ReviewUpward would be wrong here - it requires a reviewer, and
	// at transmit time naming one would be a fiction. The self-review guard in
	// ReviewRoute.Validate stays effective either way: it fires whenever a
	// reviewer IS named, which is when the authority's decision lands.
	return model.ReviewRoute{ChangeID: rec.Draft.ChangeID, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID, RequesterUserID: rec.Draft.RequesterUserID, ReviewerUserID: "", RiskLevel: risk, Mode: model.ReviewAdminQueue, Queue: r.Authority, State: model.RoutePending, OrgPath: []string{}}, nil
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

func (a NexusAuditAppender) AppendDecision(ctx context.Context, actor Actor, rec Record, in DecisionInput, operationKey string) (string, error) {
	if rec.Route.Mode != model.ReviewAdminQueue {
		return stableID("decision", actor.EnterpriseID, operationKey), nil
	}
	if a.Client == nil || (actor.UpstreamAccessToken == "" && actor.UpstreamTicketID == "") {
		return "", ErrForbidden
	}
	req := nexus.AppendAuditEvidenceRequest{
		IdempotencyKey: stableID("decision", actor.EnterpriseID, operationKey),
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
		return "", fmt.Errorf("mandatory governance decision audit: %w", err)
	}
	return out.AuditRefID, nil
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
