package governance

import (
	"context"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// fakeApprovalTransmitter records what was transmitted. It never returns a
// decision, mirroring the real surface: transmission is delivery, not approval.
type fakeApprovalTransmitter struct {
	sent      []nexusruntime.ApprovalRequest
	decision  string
	authority string
	err       error
}

func (f *fakeApprovalTransmitter) TransmitApprovalPlan(_ context.Context, req nexusruntime.ApprovalRequest) (nexusclient.ApprovalTransmissionStatus, error) {
	// Validate as the real client does, before recording. See the sibling
	// double in internal/app: a double looser than the surface it stands in for
	// is how a path that cannot work anywhere stays green everywhere.
	if err := req.Validate(); err != nil {
		return nexusclient.ApprovalTransmissionStatus{}, err
	}
	f.sent = append(f.sent, req)
	if f.err != nil {
		return nexusclient.ApprovalTransmissionStatus{}, f.err
	}
	return nexusclient.ApprovalTransmissionStatus{PlanRef: req.Plan.PlanRef, Status: nexusclient.TransmissionDelivered}, nil
}

func (f *fakeApprovalTransmitter) TransmitApprovalPlanWithBearer(ctx context.Context, _ string, req nexusruntime.ApprovalRequest) (nexusclient.ApprovalTransmissionStatus, error) {
	return f.TransmitApprovalPlan(ctx, req)
}

// decision, when set, is what a refresh will observe. Empty means the
// authority has not decided yet - a state, not a failure.
func (f *fakeApprovalTransmitter) GetApprovalTransmission(_ context.Context, planRef string) (nexusclient.ApprovalTransmissionStatus, error) {
	return nexusclient.ApprovalTransmissionStatus{
		PlanRef: planRef, Status: nexusclient.TransmissionEvidenceRecorded,
		Decision: f.decision, Authority: f.authority,
	}, nil
}
