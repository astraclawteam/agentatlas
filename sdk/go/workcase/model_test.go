package workcase_test

import (
	"errors"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/workcase"
)

func validPlan() workcase.WorkPlan {
	return workcase.WorkPlan{
		Revision: 1,
		Steps: []workcase.Step{{
			ID: "step-1",
			Action: &workcase.ActionSpec{
				Kind: "read", BusinessCapability: "mes.anomaly.read",
				ParametersHash: "sha256:fixture", IdempotencyKey: "case-1:step-1:v1",
				FailurePolicy: "retry_then_human",
			},
		}},
	}
}

func TestWorkPlanRejectsSideEffectWithoutCompensationPolicy(t *testing.T) {
	plan := validPlan()
	plan.Steps[0].Action = &workcase.ActionSpec{
		Kind: "write", BusinessCapability: "mes.work_order.create",
		IdempotencyKey: "case-1:step-1:v1",
	}
	err := workcase.ValidatePlan(plan)
	if !errors.Is(err, workcase.ErrMissingFailurePolicy) {
		t.Fatalf("got %v", err)
	}
}

func TestValidatePlanAcceptsWriteWithFailurePolicy(t *testing.T) {
	plan := validPlan()
	plan.Steps[0].Action = &workcase.ActionSpec{
		Kind: "write", BusinessCapability: "mes.work_order.create",
		ParametersHash: "sha256:fixture", IdempotencyKey: "case-1:step-1:v1",
		FailurePolicy: "compensate_then_human",
	}
	if err := workcase.ValidatePlan(plan); err != nil {
		t.Fatalf("write action with failure policy must validate, got %v", err)
	}
}

func TestValidatePlanAcceptsReadWithoutFailurePolicy(t *testing.T) {
	plan := validPlan()
	plan.Steps[0].Action.FailurePolicy = ""
	if err := workcase.ValidatePlan(plan); err != nil {
		t.Fatalf("only kind \"write\" is gated on failure policy, got %v", err)
	}
}

func TestValidatePlanAcceptsCheckpointStepWithNilAction(t *testing.T) {
	plan := validPlan()
	plan.Steps[0].Action = nil
	if err := workcase.ValidatePlan(plan); err != nil {
		t.Fatalf("checkpoint step with nil action must validate, got %v", err)
	}
}

func TestValidatePlanAcceptsEmptyPlan(t *testing.T) {
	if err := workcase.ValidatePlan(workcase.WorkPlan{Revision: 1}); err != nil {
		t.Fatalf("plan with no steps must validate, got %v", err)
	}
}
