package governance

import (
	"fmt"
	"time"
	"unicode/utf8"
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
	for name, value := range map[string]string{"change_id": d.ChangeID, "enterprise_id": d.EnterpriseID, "org_unit_id": d.OrgUnitID, "resource_id": d.ResourceID, "requester_user_id": d.RequesterUserID} {
		if value == "" || utf8.RuneCountInString(value) > 128 {
			return fmt.Errorf("%s must contain 1..128 characters", name)
		}
	}
	if !validResourceType(d.ResourceType) || !validAction(d.Action) || !validChangeState(d.State) {
		return fmt.Errorf("change draft contains an invalid enum")
	}
	if d.Revision < 1 || d.BaseVersion < 0 {
		return fmt.Errorf("revision must be positive and base_version non-negative")
	}
	if d.ProposedContent == nil || len(d.ProposedContent) > 100 {
		return fmt.Errorf("proposed_content must contain at most 100 properties")
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		return fmt.Errorf("created_at and updated_at are required")
	}
	if d.Origin == OriginDirectEdit && d.PermissionMode != PermissionDirectEdit {
		return fmt.Errorf("direct edits require direct_edit permission mode")
	}
	if d.Origin == OriginEmployeeSuggestion && (d.PermissionMode != PermissionSuggestionOnly || d.Action == ActionPublish) {
		return fmt.Errorf("employee suggestions are suggestion-only and cannot publish")
	}
	if d.Origin != OriginDirectEdit && d.Origin != OriginEmployeeSuggestion {
		return fmt.Errorf("invalid change origin")
	}
	return nil
}

type RiskAssessment struct {
	RiskLevel   RiskLevel `json:"risk_level"`
	RiskReasons []string  `json:"risk_reasons"`
}

func (r RiskAssessment) Validate() error {
	if r.RiskLevel != RiskLow && r.RiskLevel != RiskHigh {
		return fmt.Errorf("invalid risk level")
	}
	if r.RiskReasons == nil || len(r.RiskReasons) > 100 {
		return fmt.Errorf("too many risk reasons")
	}
	seen := map[string]bool{}
	for _, reason := range r.RiskReasons {
		if reason == "" || utf8.RuneCountInString(reason) > 256 || seen[reason] {
			return fmt.Errorf("risk reasons must be unique strings of 1..256 characters")
		}
		seen[reason] = true
	}
	return nil
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
	for name, value := range map[string]string{"change_id": r.ChangeID, "resource_id": r.ResourceID, "requester_user_id": r.RequesterUserID} {
		if value == "" || utf8.RuneCountInString(value) > 128 {
			return fmt.Errorf("%s must contain 1..128 characters", name)
		}
	}
	if !validResourceType(r.ResourceType) || !validRiskLevel(r.RiskLevel) || !validRouteState(r.State) {
		return fmt.Errorf("review route contains an invalid enum")
	}
	if r.OrgPath == nil || len(r.OrgPath) > 100 {
		return fmt.Errorf("org_path is required and bounded to 100 entries")
	}
	seen := map[string]bool{}
	for _, item := range r.OrgPath {
		if item == "" || utf8.RuneCountInString(item) > 128 || seen[item] {
			return fmt.Errorf("org_path entries must be unique strings of 1..128 characters")
		}
		seen[item] = true
	}
	if utf8.RuneCountInString(r.ReviewerUserID) > 128 || utf8.RuneCountInString(r.Queue) > 128 {
		return fmt.Errorf("reviewer_user_id and queue are bounded to 128 characters")
	}
	if r.ReviewerUserID != "" && r.RequesterUserID == r.ReviewerUserID {
		return fmt.Errorf("requester cannot review their own change")
	}
	switch r.Mode {
	case ReviewSingleConfirmation:
		if r.RiskLevel != RiskLow || r.ReviewerUserID != "" || r.Queue != "" {
			return fmt.Errorf("single confirmation is low risk and has no reviewer or queue")
		}
	case ReviewUpward:
		if (r.RiskLevel != RiskLow && r.RiskLevel != RiskHigh) || r.ReviewerUserID == "" || len(r.OrgPath) == 0 || r.Queue != "" {
			return fmt.Errorf("upward review requires low/high risk, reviewer, and org path")
		}
	case ReviewAdminQueue:
		if (r.RiskLevel != RiskLow && r.RiskLevel != RiskHigh) || r.Queue == "" || r.ReviewerUserID != "" {
			return fmt.Errorf("admin queue requires low/high risk and queue")
		}
	default:
		return fmt.Errorf("unknown review mode")
	}
	return nil
}

func validResourceType(v ResourceType) bool {
	switch v {
	case ResourceKnowledgeEntry, ResourceSOP, ResourceWorkflow, ResourceDreamPolicy, ResourceMethodOutline:
		return true
	}
	return false
}
func validAction(v Action) bool {
	switch v {
	case ActionCreate, ActionUpdate, ActionPublish, ActionDisable, ActionDelete:
		return true
	}
	return false
}
func validChangeState(v ChangeState) bool {
	switch v {
	case ChangeDraftState, ChangeSubmitted, ChangeApproved, ChangeRejected, ChangePublished, ChangeWithdrawn:
		return true
	}
	return false
}
func validRiskLevel(v RiskLevel) bool { return v == RiskLow || v == RiskHigh }
func validRouteState(v RouteState) bool {
	switch v {
	case RoutePending, RouteApproved, RouteRejected, RouteCancelled:
		return true
	}
	return false
}
