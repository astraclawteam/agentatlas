package auditrefs

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

type failingNexus struct {
	nexusclient.Mock
	mode string // "error" | "empty" | "ok"
}

func (f *failingNexus) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	switch f.mode {
	case "error":
		return nexus.AppendAuditEvidenceResponse{}, errors.New("chain down")
	case "empty":
		return nexus.AppendAuditEvidenceResponse{}, nil
	default:
		return f.Mock.AppendAuditEvidence(ctx, req)
	}
}

func TestAppenderCountsFailures(t *testing.T) {
	metrics := observability.NewMetrics()
	req := nexus.AppendAuditEvidenceRequest{
		BusinessContextRef: "t", Action: nexus.AuditAnswerTraceCreated,
	}

	down := New(&failingNexus{Mock: *nexusclient.NewMock(), mode: "error"}, metrics)
	if _, err := down.AppendAuditEvidence(context.Background(), req); err == nil {
		t.Fatal("error must propagate (fail closed)")
	}
	empty := New(&failingNexus{Mock: *nexusclient.NewMock(), mode: "empty"}, metrics)
	if _, err := empty.AppendAuditEvidence(context.Background(), req); err == nil {
		t.Fatal("empty ref must fail closed")
	}
	if got := testutil.ToFloat64(metrics.AuditAppendFail); got != 2 {
		t.Fatalf("failure counter = %v, want 2", got)
	}

	ok := New(&failingNexus{Mock: *nexusclient.NewMock(), mode: "ok"}, metrics)
	resp, err := ok.AppendAuditEvidence(context.Background(), req)
	if err != nil || resp.AuditRefID == "" {
		t.Fatalf("success path: %+v %v", resp, err)
	}
	if got := testutil.ToFloat64(metrics.AuditAppendFail); got != 2 {
		t.Fatalf("success must not increment failures: %v", got)
	}
}

func TestMustAuditCoversAnswerPath(t *testing.T) {
	for _, action := range []nexus.AuditAction{
		nexus.AuditAnswerTraceCreated, nexus.AuditEvidenceRead, nexus.AuditWorkflowVersionPublished,
		nexus.AuditDreamPolicyCreated, nexus.AuditDreamPolicyCreateRequested,
	} {
		if !MustAuditActions[action] {
			t.Fatalf("%s must be in the must-audit set", action)
		}
	}
}
