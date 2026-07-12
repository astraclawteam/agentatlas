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

type fakeBrowserNexus struct{ route nexus.ApprovalRoute }

func (f fakeBrowserNexus) AuthorizeBrowserOperation(context.Context, string, nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{}, nil
}
func (f fakeBrowserNexus) ResolveApprovalRouteWithBearer(context.Context, string, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return f.route, nil
}
func (f fakeBrowserNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}
