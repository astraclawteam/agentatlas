package app

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
	// Validate exactly as the real client does, BEFORE recording the send.
	// Omitting this is what let the whole transmit path look green while every
	// real call was rejected locally: the production client calls
	// req.Validate() before it ever reaches the network, and this double did
	// not, so the tests were exercising a surface strictly more permissive than
	// the one that ships.
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

// alwaysWorkCaseBacked makes a governed change look WorkCase-backed so the
// transmit-and-refresh path stays exercisable. Production changes are pol_*
// today, so without this every governance test would only ever cover the
// dormant branch and the shipping transmission code would rot untested until
// C1 lands.
func alwaysWorkCaseBacked(changeID string) (string, bool) {
	return "wc_" + changeID + "0000000000000000", true
}
