// Package nexus defines the client contract AgentAtlas consumes from
// AgentNexus. This mirrors api/openapi/agentnexus-client.yaml and its frozen
// published-contract fixtures. All enterprise resource
// access in AgentAtlas must flow through an implementation of Client; no
// module may reach enterprise data sources directly.
package nexus

import (
	"context"
	"time"
)

type VerifyTicketRequest struct {
	TicketID string `json:"ticket_id"`
}

type VerifyTicketResponse struct {
	Valid        bool      `json:"valid"`
	EnterpriseID string    `json:"enterprise_id,omitempty"`
	ActorUserID  string    `json:"actor_user_id,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

type LocateEvidenceRequest struct {
	TicketID          string `json:"ticket_id"`
	EnterpriseID      string `json:"enterprise_id"`
	EvidencePointerID string `json:"evidence_pointer_id,omitempty"`
	ResourceURI       string `json:"resource_uri,omitempty"`
	QueryIntent       string `json:"query_intent,omitempty"`
}

type LocateEvidenceResponse struct {
	ResourceURI   string         `json:"resource_uri"`
	SourceSystem  string         `json:"source_system"`
	LocationHints map[string]any `json:"location_hints,omitempty"`
}

type ReadEvidenceRequest struct {
	TicketID          string `json:"ticket_id"`
	EnterpriseID      string `json:"enterprise_id"`
	ResourceURI       string `json:"resource_uri"`
	EvidencePointerID string `json:"evidence_pointer_id,omitempty"`
	MaxBytes          int    `json:"max_bytes,omitempty"`
}

type ReadEvidenceResponse struct {
	GrantID          string `json:"grant_id"`
	ContentType      string `json:"content_type"`
	SanitizedExcerpt string `json:"sanitized_excerpt"`
	ContentHash      string `json:"content_hash"`
}

// AuditAction enumerates the AgentAtlas actions that must reach the
// AgentNexus audit chain.
type AuditAction string

const (
	AuditWorkflowDraftCreated       AuditAction = "workflow_draft_created"
	AuditWorkflowVersionPublished   AuditAction = "workflow_version_published"
	AuditDreamPolicyCreated         AuditAction = "dream_policy_created"
	AuditDreamPolicyCreateRequested AuditAction = "dream_policy_create_requested"
	AuditDreamJobRun                AuditAction = "dream_job_run"
	AuditRetrievalPlanCreated       AuditAction = "retrieval_plan_created"
	AuditEvidenceLocated            AuditAction = "evidence_located"
	AuditEvidenceRead               AuditAction = "evidence_read"
	AuditAnswerTraceCreated         AuditAction = "answer_trace_created"
	AuditSensitiveArtifactParsed    AuditAction = "sensitive_artifact_parsed"
	AuditVisibilityRuleChanged      AuditAction = "visibility_rule_changed"
)

type AppendAuditEvidenceRequest struct {
	IdempotencyKey string         `json:"-"`
	TicketID       string         `json:"ticket_id"`
	EnterpriseID   string         `json:"enterprise_id"`
	Action         AuditAction    `json:"action"`
	ResourceType   string         `json:"resource_type"`
	ResourceID     string         `json:"resource_id"`
	TraceID        string         `json:"trace_id,omitempty"`
	Details        map[string]any `json:"details,omitempty"`
}

type AppendAuditEvidenceResponse struct {
	AuditRefID string `json:"audit_ref_id"`
}

type OrgScopeKind string

const (
	ScopeEmployee     OrgScopeKind = "employee"
	ScopeProjectGroup OrgScopeKind = "project_group"
	ScopeDepartment   OrgScopeKind = "department"
	ScopeBusinessUnit OrgScopeKind = "business_unit"
	ScopeCompany      OrgScopeKind = "company"
)

type OrgEventType string

const (
	OrgEmployeeUpserted     OrgEventType = "employee_upserted"
	OrgEmployeeRemoved      OrgEventType = "employee_removed"
	OrgProjectGroupUpserted OrgEventType = "project_group_upserted"
	OrgProjectGroupRemoved  OrgEventType = "project_group_removed"
	OrgDepartmentUpserted   OrgEventType = "department_upserted"
	OrgDepartmentRemoved    OrgEventType = "department_removed"
	OrgBusinessUnitUpserted OrgEventType = "business_unit_upserted"
	OrgBusinessUnitRemoved  OrgEventType = "business_unit_removed"
	OrgCompanyUpserted      OrgEventType = "company_upserted"
)

type OrgScope struct {
	Kind       OrgScopeKind `json:"kind"`
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	ParentKind OrgScopeKind `json:"parent_kind,omitempty"`
	ParentID   string       `json:"parent_id,omitempty"`
}

type OrgMember struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
}

type OrgEvent struct {
	EventID      string       `json:"event_id"`
	EnterpriseID string       `json:"enterprise_id"`
	OrgVersion   int64        `json:"org_version"`
	Type         OrgEventType `json:"type"`
	Scope        OrgScope     `json:"scope"`
	Members      []OrgMember  `json:"members,omitempty"`
	OccurredAt   time.Time    `json:"occurred_at"`
}

// OrgEventHandler processes one event; returning an error stops the
// subscription so the caller can resume from the last processed org_version.
type OrgEventHandler func(ctx context.Context, event OrgEvent) error

// Client is the only doorway from AgentAtlas to enterprise resources.
type Client interface {
	VerifyTicket(ctx context.Context, req VerifyTicketRequest) (VerifyTicketResponse, error)
	LocateEvidence(ctx context.Context, req LocateEvidenceRequest) (LocateEvidenceResponse, error)
	ReadEvidence(ctx context.Context, req ReadEvidenceRequest) (ReadEvidenceResponse, error)
	AppendAuditEvidence(ctx context.Context, req AppendAuditEvidenceRequest) (AppendAuditEvidenceResponse, error)
	SubscribeOrgEvents(ctx context.Context, enterpriseID string, sinceVersion int64, handler OrgEventHandler) error
}

// ApprovalClient is the frozen AgentNexus /v1/approvals/resolve subset. It is
// separate from Client so older evidence-only test doubles remain compatible.
type ApprovalClient interface {
	ResolveApprovalRoute(ctx context.Context, req ApprovalResolveRequest) (ApprovalRoute, error)
}

type ApprovalResolveRequest struct {
	TicketID                string    `json:"-"`
	EnterpriseID            string    `json:"-"`
	ActorUserID             string    `json:"-"`
	IdempotencyKey          string    `json:"-"`
	OrgVersion              int64     `json:"org_version"`
	OrgUnitID               string    `json:"org_unit_id"`
	ResourceType            string    `json:"resource_type"`
	ResourceID              string    `json:"resource_id"`
	Action                  string    `json:"action"`
	ChangedFields           []string  `json:"changed_fields"`
	ImpactedOrgUnitIDs      []string  `json:"impacted_org_unit_ids"`
	ImpactedUserCount       int       `json:"impacted_user_count"`
	PublishedBehaviorChange bool      `json:"published_behavior_change"`
	ExternalSideEffect      bool      `json:"external_side_effect"`
	RequestedRisk           string    `json:"requested_risk"`
	FactsIssuedAt           time.Time `json:"facts_issued_at"`
	FactsExpiresAt          time.Time `json:"facts_expires_at"`
	FactsNonce              string    `json:"facts_nonce"`
}

type ApprovalRoute struct {
	Mode                string   `json:"mode"`
	RiskLevel           string   `json:"risk_level"`
	RiskReasons         []string `json:"risk_reasons"`
	RequesterUserID     string   `json:"requester_user_id"`
	ReviewerUserID      string   `json:"reviewer_user_id,omitempty"`
	ReviewerDisplayName string   `json:"reviewer_display_name,omitempty"`
	OrgPath             []string `json:"org_path"`
	Queue               string   `json:"queue,omitempty"`
	AutoPublish         bool     `json:"auto_publish"`
}
