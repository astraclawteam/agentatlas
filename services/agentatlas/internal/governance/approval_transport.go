package governance

import (
	"context"
	"fmt"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// ApprovalTransmitter delivers a caller-authored approval plan to AgentNexus.
//
// It is declared here rather than in sdk/go/nexus because that module has no
// dependency on the AgentNexus SDK and must keep none: adding one would make
// AgentAtlas's own published SDK carry a sibling replace directive.
//
// There is no Resolve counterpart on purpose. AgentNexus transmits and
// validates; it does not choose approvers, so there is nothing to resolve.
type ApprovalTransmitter interface {
	TransmitApprovalPlan(ctx context.Context, req nexusruntime.ApprovalRequest) (nexusclient.ApprovalTransmissionStatus, error)
	TransmitApprovalPlanWithBearer(ctx context.Context, accessToken string, req nexusruntime.ApprovalRequest) (nexusclient.ApprovalTransmissionStatus, error)
	// GetApprovalTransmission reads the current state of a transmitted plan.
	// This is how a reviewer becomes known: the decision arrives after
	// delivery, so a caller that needs one asks here rather than at transmit.
	GetApprovalTransmission(ctx context.Context, planRef string) (nexusclient.ApprovalTransmissionStatus, error)
}

// ApprovalPlanRefFor derives the stable plan handle for one change revision.
// Transmit and refresh must agree on it, so it is computed in one place rather
// than spelled out at each call site.
func ApprovalPlanRefFor(enterpriseID, changeID string, revision uint64) string {
	return stableID("apl", enterpriseID, changeID, fmt.Sprint(revision))
}

// approvalPlanTTL bounds a transmitted plan. The frozen contract requires an
// explicit expiry: an approval request with no deadline is a standing
// authorization.
const approvalPlanTTL = 24 * time.Hour
