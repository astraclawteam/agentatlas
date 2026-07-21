package governance

import (
	"context"
	"fmt"
	"strings"
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

// WorkCaseContextFor reports the WorkCase handle a governed change belongs to,
// and whether it has one at all.
//
// The frozen ApprovalRequest validates business_context_ref as an opaque wc_*
// WorkCase handle, and the client calls Validate() before the request reaches
// the network. Governed changes are not WorkCase-backed yet -- the WorkCase
// orchestrator is task C1 -- so today this is always (", false), and every
// transmit of a dream policy or a change id would be rejected locally with
// `"pol_..." is not an opaque wc_* handle`.
//
// This exists so that fact is stated ONCE and both governance paths branch on
// it, instead of each one independently building a request that cannot be
// sent. When C1 lands, a governed change gains a WorkCase and this returns its
// handle; nothing else about the transmit path has to change.
func WorkCaseContextFor(changeID string) (string, bool) {
	if strings.HasPrefix(changeID, "wc_") {
		return changeID, true
	}
	return "", false
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
