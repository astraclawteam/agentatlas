package nexusclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// The frozen approval TRANSMISSION surface.
//
// AgentNexus retired approval resolution: it no longer chooses approvers. It
// transmits a caller-authored plan to the deployment's approval authority (the
// customer's OA/BPM system) and validates the evidence that comes back.
//
// The consequence for AgentAtlas is not a renamed endpoint. The call stops
// being synchronous: transmitting a plan yields a transmission status, never an
// assigned reviewer. The reviewer becomes known only when the authority returns
// evidence.
// ApprovalTransmissionStatus mirrors the frozen ApprovalTransmissionStatus
// schema exactly.
//
// Two of its properties are load-bearing and easy to get wrong:
//
// Status is MONOTONIC (pending -> delivered -> evidence_recorded, with revoked
// terminal) and carries no grant state. "delivered" means the plan reached the
// authority, NOT that anyone decided; only evidence_recorded means a signed
// decision exists.
//
// There is deliberately no approver identity here. The schema says so in as
// many words - "No approver identity appears here by boundary" - and
// ApprovalEvidence.approver_authority repeats it: an authority, "never an
// approver identity". Who inside the customer's OA/BPM clicked approve is that
// system's business and does not cross this contract.
type ApprovalTransmissionStatus struct {
	PlanRef            string `json:"plan_ref"`
	PlanHash           string `json:"plan_hash"`
	Authority          string `json:"authority"`
	BusinessContextRef string `json:"business_context_ref"`
	Capability         string `json:"capability"`
	ParameterHash      string `json:"parameter_hash"`
	Status             string `json:"status"`
	ExpiresAt          string `json:"expires_at"`
	DeliveryAttempts   int    `json:"delivery_attempts"`
	LastDeliveryState  string `json:"last_delivery_state,omitempty"`
	Decision           string `json:"decision,omitempty"`
	DecidedAt          string `json:"decided_at,omitempty"`
	RevokedAt          string `json:"revoked_at,omitempty"`
	UpdatedAt          string `json:"updated_at"`
}

// Frozen ApprovalTransmissionStatus.status values.
const (
	TransmissionPending          = "pending"
	TransmissionDelivered        = "delivered"
	TransmissionEvidenceRecorded = "evidence_recorded"
	TransmissionRevoked          = "revoked"
)

// Frozen ApprovalDecision values.
const (
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalNarrowed = "narrowed"
)

// Decided reports whether a signed decision exists. Only evidence_recorded
// carries one: delivered means the plan arrived, not that anyone acted on it.
func (s ApprovalTransmissionStatus) Decided() bool {
	return s.Status == TransmissionEvidenceRecorded && s.Decision != ""
}

// TransmitApprovalPlan hands a caller-authored plan to AgentNexus for delivery.
// It returns a transmission status, never a decision: AgentNexus does not
// decide, and neither does this call.
func (c *HTTPClient) TransmitApprovalPlan(ctx context.Context, req nexusruntime.ApprovalRequest) (ApprovalTransmissionStatus, error) {
	var out ApprovalTransmissionStatus
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus approval request: %w", err)
	}
	err := c.post(ctx, "/v1/approvals/transmissions", req, &out)
	return out, err
}

// GetApprovalTransmission reads the current state of a transmitted plan.
//
// This is how a reviewer becomes known. Transmission is delivery, so the
// decision arrives later; polling this is the consumer's half of that
// asynchrony. An unfinished transmission is not an error - it is a state.
func (c *HTTPClient) GetApprovalTransmission(ctx context.Context, planRef string) (ApprovalTransmissionStatus, error) {
	var out ApprovalTransmissionStatus
	if planRef == "" {
		return out, fmt.Errorf("nexus approval transmission: plan_ref is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/approvals/transmissions/"+url.PathEscape(planRef), nil)
	if err != nil {
		return out, fmt.Errorf("nexus approval transmission: %w", err)
	}
	req.SetBasicAuth(c.serviceClientID, c.serviceSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("nexus approval transmission: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return out, fmt.Errorf("nexus approval transmission: %w", ErrDenied)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("nexus approval transmission: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("nexus approval transmission: decode: %w", err)
	}
	return out, nil
}

// TransmitApprovalPlanWithBearer is the browser-session variant.
func (c *HTTPClient) TransmitApprovalPlanWithBearer(ctx context.Context, accessToken string, req nexusruntime.ApprovalRequest) (ApprovalTransmissionStatus, error) {
	var out ApprovalTransmissionStatus
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus approval request: %w", err)
	}
	err := c.bearerPost(ctx, "/v1/approvals/transmissions", accessToken, "", req, &out, nil)
	return out, err
}
