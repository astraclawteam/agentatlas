package governance

import (
	"fmt"
	"time"
)

type ResourceType string

const (
	ResourceKnowledgeEntry ResourceType = "knowledge_entry"
	ResourceSOP            ResourceType = "sop"
	ResourceWorkflow       ResourceType = "workflow"
	ResourceDreamPolicy    ResourceType = "dream_policy"
	ResourceMethodOutline  ResourceType = "method_outline"
)

type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionPublish Action = "publish"
	ActionDisable Action = "disable"
	ActionDelete  Action = "delete"
)

type ChangeState string

const (
	ChangeDraftState ChangeState = "draft"
	ChangeSubmitted  ChangeState = "submitted"
	ChangeApproved   ChangeState = "approved"
	ChangeRejected   ChangeState = "rejected"
	ChangePublished  ChangeState = "published"
	ChangeWithdrawn  ChangeState = "withdrawn"
)

type ChangeOrigin string

const (
	OriginDirectEdit         ChangeOrigin = "direct_edit"
	OriginEmployeeSuggestion ChangeOrigin = "employee_suggestion"
)

type PermissionMode string

const (
	PermissionDirectEdit     PermissionMode = "direct_edit"
	PermissionSuggestionOnly PermissionMode = "suggestion_only"
)

type RiskLevel string

const (
	RiskLow  RiskLevel = "low"
	RiskHigh RiskLevel = "high"
)

type ReviewMode string

const (
	ReviewSingleConfirmation ReviewMode = "single_confirmation"
	ReviewUpward             ReviewMode = "upward_review"
	ReviewAdminQueue         ReviewMode = "enterprise_knowledge_admin_queue"
)

type RouteState string

const (
	RoutePending   RouteState = "pending"
	RouteApproved  RouteState = "approved"
	RouteRejected  RouteState = "rejected"
	RouteCancelled RouteState = "cancelled"
)

type ChangeDraft struct {
	ChangeID        string         `json:"change_id"`
	EnterpriseID    string         `json:"enterprise_id"`
	OrgUnitID       string         `json:"org_unit_id"`
	ResourceType    ResourceType   `json:"resource_type"`
	ResourceID      string         `json:"resource_id"`
	Action          Action         `json:"action"`
	RequesterUserID string         `json:"requester_user_id"`
	Origin          ChangeOrigin   `json:"origin"`
	PermissionMode  PermissionMode `json:"permission_mode"`
	Revision        int32          `json:"revision"`
	State           ChangeState    `json:"state"`
	BaseVersion     int32          `json:"base_version"`
	ProposedContent map[string]any `json:"proposed_content"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

func (d ChangeDraft) Validate() error {
	if d.Origin == OriginEmployeeSuggestion && (d.PermissionMode != PermissionSuggestionOnly || d.Action == ActionPublish) {
		return fmt.Errorf("employee suggestions are suggestion-only and cannot publish")
	}
	return nil
}

type RiskAssessment struct {
	RiskLevel   RiskLevel `json:"risk_level"`
	RiskReasons []string  `json:"risk_reasons"`
}
type ReviewRoute struct {
	ChangeID        string       `json:"change_id"`
	ResourceType    ResourceType `json:"resource_type"`
	ResourceID      string       `json:"resource_id"`
	RequesterUserID string       `json:"requester_user_id"`
	ReviewerUserID  string       `json:"reviewer_user_id,omitempty"`
	RiskLevel       RiskLevel    `json:"risk_level"`
	Mode            ReviewMode   `json:"mode"`
	State           RouteState   `json:"state"`
	OrgPath         []string     `json:"org_path"`
	Queue           string       `json:"queue,omitempty"`
}

func (r ReviewRoute) Validate() error {
	if r.ReviewerUserID != "" && r.RequesterUserID == r.ReviewerUserID {
		return fmt.Errorf("requester cannot review their own change")
	}
	switch r.Mode {
	case ReviewSingleConfirmation:
		if r.RiskLevel != RiskLow || r.ReviewerUserID != "" || r.Queue != "" {
			return fmt.Errorf("single confirmation is low risk and has no reviewer or queue")
		}
	case ReviewUpward:
		if r.RiskLevel != RiskHigh || r.ReviewerUserID == "" || len(r.OrgPath) == 0 || r.Queue != "" {
			return fmt.Errorf("upward review requires high risk, reviewer, and org path")
		}
	case ReviewAdminQueue:
		if r.RiskLevel != RiskHigh || r.Queue == "" || r.ReviewerUserID != "" {
			return fmt.Errorf("admin queue requires high risk and queue")
		}
	default:
		return fmt.Errorf("unknown review mode")
	}
	return nil
}
