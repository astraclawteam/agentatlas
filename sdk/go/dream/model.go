package dream

import "time"

type Source string

const (
	SourceWorkBrief         Source = "work_brief"
	SourceChildDreamSummary Source = "child_dream_summary"
	SourceProjectRecord     Source = "project_record"
	SourceSOPUpdate         Source = "sop_update"
	SourceAgentAnswer       Source = "agent_answer"
	SourceExternalEvidence  Source = "external_evidence"
	SourceCompletedTask     Source = "completed_task"
	SourceRiskEvent         Source = "risk_event"
)

type WorkflowRef struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
}

type SignalSeverity string

const (
	SignalSeverityInfo     SignalSeverity = "info"
	SignalSeverityWarning  SignalSeverity = "warning"
	SignalSeverityCritical SignalSeverity = "critical"
)

type StructuredSignal struct {
	ID                string         `json:"id"`
	Title             string         `json:"title"`
	Detail            string         `json:"detail"`
	Severity          SignalSeverity `json:"severity"`
	EvidencePointerID string         `json:"evidence_pointer_id"`
}
type Coverage struct {
	ExpectedChildren  int32 `json:"expected_children"`
	CompletedChildren int32 `json:"completed_children"`
	InputCount        int32 `json:"input_count"`
}
type MissingReason string

const (
	MissingNotFound      MissingReason = "not_found"
	MissingNotCompleted  MissingReason = "not_completed"
	MissingNotAuthorized MissingReason = "not_authorized"
	MissingFailed        MissingReason = "failed"
	MissingMasked        MissingReason = "masked"
)

type MissingInput struct {
	SourceType Source        `json:"source_type"`
	SourceID   string        `json:"source_id"`
	Reason     MissingReason `json:"reason"`
}

type SourceCount struct {
	SourceType Source `json:"source_type"`
	Count      int32  `json:"count"`
}
type InputSnapshotSummary struct {
	SourceCounts      []SourceCount `json:"source_counts"`
	SanitizedInputIDs []string      `json:"sanitized_input_ids"`
}
type VisibilityLevel string

const (
	VisibilityMembers          VisibilityLevel = "members"
	VisibilityManagers         VisibilityLevel = "managers"
	VisibilityCompanySanitized VisibilityLevel = "company_sanitized"
)

type VisibilitySnapshotSummary struct {
	VisibilityLevel  VisibilityLevel `json:"visibility_level"`
	OrgUnitIDs       []string        `json:"org_unit_ids"`
	MaskedFieldCount int32           `json:"masked_field_count"`
}

type ConfirmationMode string

const (
	ConfirmationAlways       ConfirmationMode = "always"
	ConfirmationHighRiskOnly ConfirmationMode = "high_risk_only"
	ConfirmationNever        ConfirmationMode = "never"
)

type EvidenceRetention string

const (
	EvidencePointerOnly               EvidenceRetention = "pointer_only"
	EvidencePointerPlusDisplaySummary EvidenceRetention = "pointer_plus_display_summary"
)

type DreamPolicyDefinition struct {
	OrgUnitID         string            `json:"org_unit_id"`
	Timezone          string            `json:"timezone"`
	Schedule          string            `json:"schedule"`
	InputSources      []Source          `json:"input_sources"`
	Workflow          WorkflowRef       `json:"workflow"`
	OutputSpaceID     string            `json:"output_space_id"`
	VisibilityLevel   VisibilityLevel   `json:"visibility_level"`
	MaskingRules      []string          `json:"masking_rules,omitempty"`
	RiskSignalRules   []string          `json:"risk_signal_rules,omitempty"`
	EvidenceRetention EvidenceRetention `json:"evidence_retention,omitempty"`
	ConfirmationMode  ConfirmationMode  `json:"confirmation_mode"`
	MaxAttempts       int32             `json:"max_attempts,omitempty"`
}

type DreamSummaryView struct {
	DisplaySummary      string             `json:"display_summary"`
	RetrievalSummary    string             `json:"retrieval_summary"`
	SealedDetailPointer string             `json:"sealed_detail_pointer"`
	Facts               []StructuredSignal `json:"facts"`
	Themes              []StructuredSignal `json:"themes"`
	Trends              []StructuredSignal `json:"trends"`
	Risks               []StructuredSignal `json:"risks"`
	Todos               []StructuredSignal `json:"todos"`
	Coverage            Coverage           `json:"coverage"`
	MissingInputs       []MissingInput     `json:"missing_inputs"`
	EvidencePointerID   string             `json:"evidence_pointer_id"`
}

type RunStatus string

const (
	RunPending             RunStatus = "pending"
	RunRunning             RunStatus = "running"
	RunWaitingConfirmation RunStatus = "waiting_confirmation"
	RunSucceeded           RunStatus = "succeeded"
	RunFailed              RunStatus = "failed"
)

type DreamRunView struct {
	RunID              string                    `json:"run_id"`
	Status             RunStatus                 `json:"status"`
	OrgUnitID          string                    `json:"org_unit_id"`
	WindowStart        time.Time                 `json:"window_start"`
	WindowEnd          time.Time                 `json:"window_end"`
	PolicyVersion      int32                     `json:"policy_version"`
	Workflow           WorkflowRef               `json:"workflow"`
	ParentRunIDs       []string                  `json:"parent_run_ids"`
	InputCount         int32                     `json:"input_count"`
	Coverage           Coverage                  `json:"coverage"`
	MissingInputs      []MissingInput            `json:"missing_inputs"`
	Facts              []StructuredSignal        `json:"facts"`
	Themes             []StructuredSignal        `json:"themes"`
	Trends             []StructuredSignal        `json:"trends"`
	Risks              []StructuredSignal        `json:"risks"`
	Todos              []StructuredSignal        `json:"todos"`
	DisplaySummary     string                    `json:"display_summary"`
	EvidencePointerID  string                    `json:"evidence_pointer_id"`
	RerunOfRunID       string                    `json:"rerun_of_run_id,omitempty"`
	InputSnapshot      InputSnapshotSummary      `json:"input_snapshot"`
	VisibilitySnapshot VisibilitySnapshotSummary `json:"visibility_snapshot"`
	ModelRoute         string                    `json:"model_route"`
	ModelVersion       string                    `json:"model_version"`
	Attempt            int32                     `json:"attempt"`
	IdempotencyKey     string                    `json:"idempotency_key"`
}
