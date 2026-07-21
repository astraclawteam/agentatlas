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
	Valid          bool      `json:"valid"`
	EnterpriseID   string    `json:"enterprise_id,omitempty"`
	ActorUserID    string    `json:"actor_user_id,omitempty"`
	Scopes         []string  `json:"scopes,omitempty"`
	OrgVersion     int64     `json:"org_version,omitempty"`
	OrgUnitIDs     []string  `json:"org_unit_ids,omitempty"`
	ResourceType   string    `json:"resource_type,omitempty"`
	ResourceID     string    `json:"resource_id,omitempty"`
	AllowedActions []string  `json:"allowed_actions,omitempty"`
	ReviewMode     string    `json:"review_mode,omitempty"`
	Queue          string    `json:"queue,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
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
	AuditGovernanceChangeDecided    AuditAction = "governance_change_decided"
)

type AppendAuditEvidenceRequest struct {
	IdempotencyKey   string         `json:"-"`
	TicketID         string         `json:"ticket_id,omitempty"`
	EnterpriseID     string         `json:"enterprise_id"`
	Action           AuditAction    `json:"action"`
	ResourceType     string         `json:"resource_type"`
	ResourceID       string         `json:"resource_id"`
	TraceID          string         `json:"trace_id,omitempty"`
	Details          map[string]any `json:"details,omitempty"`
	OrgVersion       int64          `json:"org_version,omitempty"`
	OrgUnitID        string         `json:"org_unit_id,omitempty"`
	AuthorizedAction string         `json:"authorized_action,omitempty"`
	ReviewMode       string         `json:"review_mode,omitempty"`
	Queue            string         `json:"queue,omitempty"`
}

type AppendAuditEvidenceResponse struct {
	AuditRefID string `json:"audit_ref_id"`
}

// OrgScopeKind names a kind of organization scope. It survives the org-event
// retirement because it is not an event concept: spaces.ScopeString uses it as
// the single definition of the org_scope encoding that the brief handler and
// the dream scheduler hand-encode.
type OrgScopeKind string

const (
	ScopeEmployee     OrgScopeKind = "employee"
	ScopeProjectGroup OrgScopeKind = "project_group"
	ScopeDepartment   OrgScopeKind = "department"
	ScopeBusinessUnit OrgScopeKind = "business_unit"
	ScopeCompany      OrgScopeKind = "company"
)

// OrgEvent is one organization-graph change notification from
// GET /v1/org-events. It mirrors the frozen OrgEvent schema exactly and
// nothing more: that schema is additionalProperties:false and carries only
// these four fields.
//
// It deliberately carries NO org unit identity, scope, membership or
// enterprise id. The feed publishes only sealed versions and the contract
// states it "is a change notification only: organization payloads and source
// digests are never published here" - the narrow projection exists so the feed
// cannot become a second read path around the evidence surface.
//
// The consequence is load-bearing: a consumer can drive a VERSION CURSOR from
// this and nothing else. It cannot tell which org unit changed, so it cannot
// provision or rename anything per unit.
//
// EventType is a plain string, not an enum, because the contract requires that
// "consumers must ignore unknown values rather than fail closed on them". A
// typed enum here would turn every future event type into a hard error.
type OrgEvent struct {
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type"`
	OrgVersion int64     `json:"org_version"`
	OccurredAt time.Time `json:"occurred_at"`
}

// OrgEventHandler processes one event; returning an error stops the
// subscription so the caller can resume from the last processed org_version.
type OrgEventHandler func(ctx context.Context, event OrgEvent) error

// Client is the only doorway from AgentAtlas to enterprise resources.
type Client interface {
	VerifyTicket(ctx context.Context, req VerifyTicketRequest) (VerifyTicketResponse, error)
	AppendAuditEvidence(ctx context.Context, req AppendAuditEvidenceRequest) (AppendAuditEvidenceResponse, error)
	SubscribeOrgEvents(ctx context.Context, enterpriseID string, sinceVersion int64, handler OrgEventHandler) error
}

// ApprovalClient is the frozen AgentNexus /v1/approvals/resolve subset. It is
// separate from Client so older evidence-only test doubles remain compatible.
type ApprovalClient interface {
}

// OrgAuthorizationClient is the AgentNexus authorization-decision subset used
// when AgentAtlas holds a CaseTicket rather than a browser session.
//
// It is separate from Client for the same reason ApprovalClient is: an
// evidence-only test double stays compatible. It exists at all because two
// call sites used to answer "may this actor touch this org unit?" by calling
// evidence LOCATE with a synthesized resource URI and treating the echoed URI
// as permission. That put connector topology on the wire, used an evidence
// lookup as an authorization oracle, and inferred a grant from an echo. The
// authorization decision has its own surface, and this is it.
type OrgAuthorizationClient interface {
	AuthorizeTicketOperation(ctx context.Context, ticketID string, req BrowserAuthorizationRequest) (BrowserAuthorizationDecision, error)
}

type BrowserAuthorizationRequest struct {
	TicketID     string `json:"ticket_id,omitempty"`
	OrgUnitID    string `json:"org_unit_id"`
	OrgVersion   int64  `json:"org_version"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Action       string `json:"action"`
	ReviewMode   string `json:"review_mode,omitempty"`
	Queue        string `json:"queue,omitempty"`
}
type BrowserAuthorizationDecision struct {
	Decision    string   `json:"decision"`
	Permissions []string `json:"permissions"`
	OrgVersion  int64    `json:"org_version"`
	OrgUnitIDs  []string `json:"org_unit_ids"`
	RiskLevel   string   `json:"risk_level"`
}
type BrowserBFFClient interface {
	AuthorizeBrowserOperation(context.Context, string, BrowserAuthorizationRequest) (BrowserAuthorizationDecision, error)
	AppendAuditEvidenceWithBearer(context.Context, string, AppendAuditEvidenceRequest) (AppendAuditEvidenceResponse, error)
}

// TicketGovernanceClient is the service-credential compatibility surface.
// Every call carries the original verified CaseTicket; AgentNexus remains the
// authority for its enterprise, actor, organization, resource and action scope.
type TicketGovernanceClient interface {
	AuthorizeTicketOperation(context.Context, string, BrowserAuthorizationRequest) (BrowserAuthorizationDecision, error)
	AppendAuditEvidence(context.Context, AppendAuditEvidenceRequest) (AppendAuditEvidenceResponse, error)
}

type GovernanceClient interface {
	BrowserBFFClient
	TicketGovernanceClient
}


