// Package dream implements the organizational memory synthesis loop: dream
// policies describe how an org scope is periodically summarized; dream jobs
// aggregate work briefs into layered summaries (display / retrieval / sealed
// pointer) under visibility and masking rules.
package dream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/governance"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	publicschemas "github.com/astraclawteam/agentatlas/services/agentatlas/schemas"
)

// Policy mirrors the canonical public DreamPolicyDefinition while retaining a
// local validation method for runtime callers.
type Policy sdkdream.DreamPolicyDefinition

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Validate fails loud on anything that would make a run unsafe or undefined.
func (p Policy) Validate() error {
	p = withPolicyDefaults(p)
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := publicschemas.ValidateDreamPolicy(raw); err != nil {
		return fmt.Errorf("dream policy: %w", err)
	}
	for _, rule := range append(append([]string{}, p.MaskingRules...), p.RiskSignalRules...) {
		if _, err := regexp.Compile(rule); err != nil {
			return fmt.Errorf("dream policy: bad rule %q: %w", rule, err)
		}
	}
	return nil
}

func withPolicyDefaults(p Policy) Policy {
	if p.EvidenceRetention == "" {
		p.EvidenceRetention = sdkdream.EvidencePointerOnly
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 3
	}
	return p
}

func decodePolicy(raw []byte) (Policy, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Policy{}, err
	}
	if _, canonical := probe["org_unit_id"]; canonical {
		var p Policy
		if err := json.Unmarshal(raw, &p); err != nil {
			return Policy{}, err
		}
		p = withPolicyDefaults(p)
		if err := p.Validate(); err != nil {
			return Policy{}, err
		}
		return p, nil
	}
	var legacy struct {
		OrgScope          string   `json:"org_scope"`
		Schedule          string   `json:"schedule"`
		InputSources      []string `json:"input_sources"`
		VisibilityLevel   string   `json:"visibility_level"`
		MaskingRules      []string `json:"masking_rules"`
		RiskSignalRules   []string `json:"risk_signal_rules"`
		EvidenceRetention string   `json:"evidence_retention"`
		OutputSpaceID     string   `json:"output_space_id"`
		MaxAttempts       int32    `json:"max_attempts"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return Policy{}, err
	}
	sources := make([]sdkdream.Source, 0, len(legacy.InputSources))
	legacySources := map[string]sdkdream.Source{
		"work_briefs": sdkdream.SourceWorkBrief, "project_records": sdkdream.SourceProjectRecord,
		"sop_updates": sdkdream.SourceSOPUpdate, "agent_answers": sdkdream.SourceAgentAnswer,
		"external_evidence": sdkdream.SourceExternalEvidence,
	}
	for _, source := range legacy.InputSources {
		mapped, ok := legacySources[source]
		if !ok {
			return Policy{}, fmt.Errorf("dream policy: unknown legacy input source %q", source)
		}
		sources = append(sources, mapped)
	}
	p := Policy(sdkdream.DreamPolicyDefinition{
		OrgUnitID: legacy.OrgScope, Timezone: "UTC", Schedule: legacy.Schedule,
		InputSources: sources, Workflow: sdkdream.WorkflowRef{ID: "legacy-direct-dream", Version: 1},
		OutputSpaceID: legacy.OutputSpaceID, VisibilityLevel: sdkdream.VisibilityLevel(legacy.VisibilityLevel),
		MaskingRules: legacy.MaskingRules, RiskSignalRules: legacy.RiskSignalRules,
		EvidenceRetention: sdkdream.EvidenceRetention(legacy.EvidenceRetention),
		ConfirmationMode:  sdkdream.ConfirmationHighRiskOnly, MaxAttempts: legacy.MaxAttempts,
	})
	p = withPolicyDefaults(p)
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// PolicyStore is the persistence surface (db/generated satisfies it).
type PolicyStore interface {
	CreateDreamPolicy(ctx context.Context, arg db.CreateDreamPolicyParams) (db.DreamPolicy, error)
	GetDreamPolicy(ctx context.Context, id string) (db.DreamPolicy, error)
	UpdateDreamPolicyStatus(ctx context.Context, arg db.UpdateDreamPolicyStatusParams) (int64, error)
	PublishDreamPolicyVersion(ctx context.Context, arg db.PublishDreamPolicyVersionParams) (db.DreamPolicyVersion, error)
	GetLatestDreamPolicyVersion(ctx context.Context, policyID string) (db.DreamPolicyVersion, error)
	ListPublishedDreamPolicies(ctx context.Context, enterpriseID string) ([]db.DreamPolicy, error)
}

type policyLifecycleStore interface {
	CreateDreamPolicyLifecycle(context.Context, db.CreateDreamPolicyLifecycleParams) (db.CreateDreamPolicyLifecycleRow, error)
	GetEnterpriseDreamPolicy(context.Context, db.GetEnterpriseDreamPolicyParams) (db.DreamPolicy, error)
	UpdateDreamPolicyDraftIfRevision(context.Context, db.UpdateDreamPolicyDraftIfRevisionParams) (db.UpdateDreamPolicyDraftIfRevisionRow, error)
	SubmitDreamPolicyReviewIfRevision(context.Context, db.SubmitDreamPolicyReviewIfRevisionParams) (db.SubmitDreamPolicyReviewIfRevisionRow, error)
	DecideDreamPolicyIfRevision(context.Context, db.DecideDreamPolicyIfRevisionParams) (db.DecideDreamPolicyIfRevisionRow, error)
	PublishDreamPolicyGoverned(context.Context, db.PublishDreamPolicyGovernedParams) (db.PublishDreamPolicyGovernedRow, error)
	DisableDreamPolicyIfRevision(context.Context, db.DisableDreamPolicyIfRevisionParams) (db.DisableDreamPolicyIfRevisionRow, error)
	RefreshDreamPolicyReviewRoute(context.Context, db.RefreshDreamPolicyReviewRouteParams) (db.RefreshDreamPolicyReviewRouteRow, error)
	ReserveDreamPolicyOperation(context.Context, db.ReserveDreamPolicyOperationParams) (db.DreamPolicyOperation, error)
	GetDreamPolicyOperation(context.Context, db.GetDreamPolicyOperationParams) (db.DreamPolicyOperation, error)
	RecordDreamPolicyOperationAudit(context.Context, db.RecordDreamPolicyOperationAuditParams) (db.DreamPolicyOperation, error)
	CompleteDreamPolicyOperation(context.Context, db.CompleteDreamPolicyOperationParams) (db.DreamPolicyOperation, error)
	GetDreamPolicyTransitionAuditByOperation(context.Context, db.GetDreamPolicyTransitionAuditByOperationParams) (db.DreamPolicyTransitionAudit, error)
}

type LifecycleView struct {
	ID              string                    `json:"dream_policy_id"`
	Status          string                    `json:"status"`
	Revision        int32                     `json:"revision"`
	Version         int32                     `json:"version"`
	RequesterUserID string                    `json:"requester_user_id"`
	PermissionMode  governance.PermissionMode `json:"permission_mode"`
	RiskLevel       governance.RiskLevel      `json:"risk_level,omitempty"`
	RiskReasons     []string                  `json:"risk_reasons"`
	ReviewMode      governance.ReviewMode     `json:"review_mode,omitempty"`
	ReviewerUserID  string                    `json:"reviewer_user_id,omitempty"`
	OrgPath         []string                  `json:"org_path"`
	Queue           string                    `json:"queue,omitempty"`
	PendingAction   string                    `json:"pending_action,omitempty"`
	ReviewState     string                    `json:"review_state,omitempty"`
	Policy          Policy                    `json:"policy"`
}

func lifecycleView(row db.DreamPolicy, version int32) (LifecycleView, error) {
	p, err := decodePolicy(row.Draft)
	if err != nil {
		return LifecycleView{}, err
	}
	var reasons, path []string
	if len(row.RiskReasons) > 0 {
		if err := json.Unmarshal(row.RiskReasons, &reasons); err != nil {
			return LifecycleView{}, err
		}
	}
	if len(row.ReviewOrgPath) > 0 {
		if err := json.Unmarshal(row.ReviewOrgPath, &path); err != nil {
			return LifecycleView{}, err
		}
	}
	if reasons == nil {
		reasons = []string{}
	}
	if path == nil {
		path = []string{}
	}
	status := row.Status
	switch row.ReviewState {
	case "pending":
		status = "review_pending"
	case "approved":
		status = "approved"
	case "rejected":
		status = "rejected"
	}
	return LifecycleView{ID: row.ID, Status: status, Revision: row.Revision, Version: version,
		RequesterUserID: row.RequesterUserID, PermissionMode: governance.PermissionMode(row.PermissionMode),
		RiskLevel: governance.RiskLevel(row.RiskLevel), RiskReasons: reasons, ReviewMode: governance.ReviewMode(row.ReviewMode),
		ReviewerUserID: row.ReviewerUserID.String, OrgPath: path, Queue: row.ReviewQueue.String, PendingAction: row.PendingAction, ReviewState: row.ReviewState, Policy: p}, nil
}

type Operation struct {
	Row    db.DreamPolicyOperation
	Replay *LifecycleView
}

type ReceiptOperation struct {
	Row    db.DreamPolicyOperation
	Replay json.RawMessage
}

func (s *PolicyService) BeginReceipt(ctx context.Context, enterpriseID, key, kind, resourceID, actor, requestHash string) (ReceiptOperation, error) {
	if len(key) < 16 || len(key) > 128 || len(requestHash) != 64 || actor == "" || resourceID == "" {
		return ReceiptOperation{}, fmt.Errorf("invalid operation receipt")
	}
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return ReceiptOperation{}, fmt.Errorf("policy store does not support operation receipts")
	}
	sum := sha256.Sum256([]byte(enterpriseID + "\x00" + key))
	row, err := ls.ReserveDreamPolicyOperation(ctx, db.ReserveDreamPolicyOperationParams{EnterpriseID: enterpriseID, OperationKey: key, OperationKind: kind, PolicyID: resourceID, ActorUserID: actor, RequestHash: requestHash, FactsNonce: "facts_" + hex.EncodeToString(sum[:16])})
	if err != nil {
		return ReceiptOperation{}, fmt.Errorf("operation key conflict: %w", err)
	}
	op := ReceiptOperation{Row: row}
	if row.Status == "completed" {
		op.Replay = append(json.RawMessage(nil), row.Result...)
	}
	return op, nil
}

func (s *PolicyService) CompleteReceipt(ctx context.Context, enterpriseID, key string, result any) (json.RawMessage, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	ls := s.store.(policyLifecycleStore)
	row, err := ls.CompleteDreamPolicyOperation(ctx, db.CompleteDreamPolicyOperationParams{Result: raw, EnterpriseID: enterpriseID, OperationKey: key})
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), row.Result...), nil
}

func (s *PolicyService) BeginOperation(ctx context.Context, enterpriseID, key, kind, policyID, actor, requestHash string) (Operation, error) {
	receipt, err := s.BeginReceipt(ctx, enterpriseID, key, kind, policyID, actor, requestHash)
	if err != nil {
		return Operation{}, err
	}
	ls := s.store.(policyLifecycleStore)
	row := receipt.Row
	op := Operation{Row: row}
	if row.Status == "pending" && row.AuditRefID.Valid {
		if _, transitionErr := ls.GetDreamPolicyTransitionAuditByOperation(ctx, db.GetDreamPolicyTransitionAuditByOperationParams{EnterpriseID: enterpriseID, OperationKey: key}); transitionErr == nil {
			view, viewErr := s.GetLifecycle(ctx, enterpriseID, policyID)
			if viewErr != nil {
				return Operation{}, fmt.Errorf("reconcile lifecycle operation: %w", viewErr)
			}
			if _, completeErr := s.CompleteOperation(ctx, enterpriseID, key, view); completeErr != nil {
				return Operation{}, fmt.Errorf("reconcile lifecycle receipt: %w", completeErr)
			}
			op.Row, _ = ls.GetDreamPolicyOperation(ctx, db.GetDreamPolicyOperationParams{EnterpriseID: enterpriseID, OperationKey: key})
			op.Replay = &view
			return op, nil
		} else if !errors.Is(transitionErr, pgx.ErrNoRows) {
			return Operation{}, transitionErr
		}
	}
	if row.Status == "completed" {
		var view LifecycleView
		if err := json.Unmarshal(row.Result, &view); err != nil {
			return Operation{}, err
		}
		op.Replay = &view
	}
	return op, nil
}
func (s *PolicyService) RecordOperationAudit(ctx context.Context, enterpriseID, key, auditRef string) (db.DreamPolicyOperation, error) {
	ls := s.store.(policyLifecycleStore)
	return ls.RecordDreamPolicyOperationAudit(ctx, db.RecordDreamPolicyOperationAuditParams{AuditRefID: text(auditRef), EnterpriseID: enterpriseID, OperationKey: key})
}
func (s *PolicyService) CompleteOperation(ctx context.Context, enterpriseID, key string, view LifecycleView) (LifecycleView, error) {
	raw, err := json.Marshal(view)
	if err != nil {
		return LifecycleView{}, err
	}
	ls := s.store.(policyLifecycleStore)
	_, err = ls.CompleteDreamPolicyOperation(ctx, db.CompleteDreamPolicyOperationParams{Result: raw, EnterpriseID: enterpriseID, OperationKey: key})
	return view, err
}

func (s *PolicyService) reconcileTransition(ctx context.Context, enterpriseID, policyID, operationKey string, transitionErr error) (LifecycleView, error) {
	ls := s.store.(policyLifecycleStore)
	if _, err := ls.GetDreamPolicyTransitionAuditByOperation(ctx, db.GetDreamPolicyTransitionAuditByOperationParams{EnterpriseID: enterpriseID, OperationKey: operationKey}); err != nil {
		return LifecycleView{}, transitionErr
	}
	return s.GetLifecycle(ctx, enterpriseID, policyID)
}

// LoadVersion returns exactly one immutable published policy version.
func (s *PolicyService) LoadVersion(ctx context.Context, policyID string, version int32) (Policy, error) {
	store, ok := s.store.(interface {
		GetDreamPolicyVersion(context.Context, db.GetDreamPolicyVersionParams) (db.DreamPolicyVersion, error)
	})
	if !ok {
		return Policy{}, fmt.Errorf("policy store cannot load exact published versions")
	}
	row, err := store.GetDreamPolicyVersion(ctx, db.GetDreamPolicyVersionParams{PolicyID: policyID, Version: version})
	if err != nil {
		return Policy{}, fmt.Errorf("load policy %s@%d: %w", policyID, version, err)
	}
	return decodePolicy(row.Definition)
}

type PolicyService struct {
	store PolicyStore
}

func NewPolicyService(store PolicyStore) *PolicyService {
	return &PolicyService{store: store}
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func NewPolicyID() string { return newID("pol") }

// PolicyFixtureService is an explicit test/bootstrap-only surface. Production
// composition receives PolicyService, which has no direct publish bypass.
type PolicyFixtureService struct{ *PolicyService }

func NewPolicyFixtureService(store PolicyStore) *PolicyFixtureService {
	return &PolicyFixtureService{NewPolicyService(store)}
}
func (s *PolicyFixtureService) CreateDraft(ctx context.Context, enterpriseID string, p Policy) (string, error) {
	return s.CreateDraftWithID(ctx, enterpriseID, NewPolicyID(), p)
}

func (s *PolicyFixtureService) CreateDraftWithID(ctx context.Context, enterpriseID, policyID string, p Policy) (string, error) {
	p = withPolicyDefaults(p)
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	row, err := s.store.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{
		ID: policyID, EnterpriseID: enterpriseID, OrgScope: p.OrgUnitID,
		Status: "draft", Draft: raw,
	})
	if err != nil {
		return "", fmt.Errorf("store policy draft: %w", err)
	}
	return row.ID, nil
}

// CreateGovernedDraft stores the requester and permission mode alongside the
// version-zero draft. The audit reference must already have been durably
// appended by AgentNexus.
func (s *PolicyService) CreateGovernedDraft(ctx context.Context, enterpriseID, policyID, requester string, mode governance.PermissionMode, operationKey string, p Policy) (LifecycleView, error) {
	p = withPolicyDefaults(p)
	if err := p.Validate(); err != nil {
		return LifecycleView{}, err
	}
	if requester == "" || operationKey == "" || (mode != governance.PermissionDirectEdit && mode != governance.PermissionSuggestionOnly) {
		return LifecycleView{}, fmt.Errorf("governed Dream draft requires requester, permission mode, and audit reference")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return LifecycleView{}, err
	}
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	row, err := ls.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: policyID, EnterpriseID: enterpriseID, OrgScope: p.OrgUnitID, Draft: raw, RequesterUserID: requester, PermissionMode: string(mode), OperationKey: operationKey})
	if err != nil {
		return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("store governed policy draft: %w", err))
	}
	return lifecycleView(db.DreamPolicy{ID: row.ID, EnterpriseID: row.EnterpriseID, OrgScope: row.OrgScope, Status: row.Status, Draft: row.Draft, Revision: row.Revision, RequesterUserID: row.RequesterUserID, PermissionMode: row.PermissionMode, PendingAction: row.PendingAction, ReviewState: row.ReviewState, RiskLevel: row.RiskLevel, RiskReasons: row.RiskReasons, ReviewMode: row.ReviewMode, ReviewerUserID: row.ReviewerUserID, ReviewOrgPath: row.ReviewOrgPath, ReviewQueue: row.ReviewQueue, Decision: row.Decision, AuditRefID: row.AuditRefID}, 0)
}

func (s *PolicyService) GetLifecycle(ctx context.Context, enterpriseID, policyID string) (LifecycleView, error) {
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	row, err := ls.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: enterpriseID, ID: policyID})
	if err != nil {
		return LifecycleView{}, err
	}
	version := int32(0)
	if latest, latestErr := s.store.GetLatestDreamPolicyVersion(ctx, policyID); latestErr == nil {
		version = latest.Version
	}
	return lifecycleView(row, version)
}

func (s *PolicyService) OrgVersion(ctx context.Context, enterpriseID string) (int64, error) {
	store, ok := s.store.(interface {
		GetDreamOrgTreeVersion(context.Context, string) (int64, error)
	})
	if !ok {
		return 0, fmt.Errorf("Dream organization version unavailable")
	}
	version, err := store.GetDreamOrgTreeVersion(ctx, enterpriseID)
	if err != nil {
		return 0, err
	}
	if version < 1 {
		return 0, fmt.Errorf("Dream organization snapshot unavailable")
	}
	return version, nil
}

func (s *PolicyService) UpdateGovernedDraft(ctx context.Context, enterpriseID, policyID, actor, auditRef, operationKey string, revision int32, p Policy) (LifecycleView, error) {
	p = withPolicyDefaults(p)
	if err := p.Validate(); err != nil {
		return LifecycleView{}, err
	}
	if actor == "" || auditRef == "" {
		return LifecycleView{}, fmt.Errorf("update requires actor and durable audit reference")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return LifecycleView{}, err
	}
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	row, err := ls.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: p.OrgUnitID, Draft: raw, AuditRefID: text(auditRef), TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision, ActorUserID: actor, OperationKey: operationKey})
	if err != nil {
		return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("update Dream policy revision: %w", err))
	}
	_ = row
	return s.GetLifecycle(ctx, enterpriseID, policyID)
}

// Assess returns deterministic risk facts. Creation has no published baseline
// and is therefore high risk. Updates are high risk when any security or
// execution-binding field changes.
func (s *PolicyService) Assess(ctx context.Context, enterpriseID, policyID string) (governance.RiskAssessment, []string, LifecycleView, error) {
	view, err := s.GetLifecycle(ctx, enterpriseID, policyID)
	if err != nil {
		return governance.RiskAssessment{}, nil, LifecycleView{}, err
	}
	changed := []string{}
	latest, latestErr := s.store.GetLatestDreamPolicyVersion(ctx, policyID)
	if latestErr != nil {
		changed = []string{"org_unit_id", "visibility_level", "masking_rules", "evidence_retention", "workflow", "confirmation_mode"}
	} else {
		base, err := decodePolicy(latest.Definition)
		if err != nil {
			return governance.RiskAssessment{}, nil, LifecycleView{}, err
		}
		if base.OrgUnitID != view.Policy.OrgUnitID {
			changed = append(changed, "org_unit_id")
		}
		if base.VisibilityLevel != view.Policy.VisibilityLevel {
			changed = append(changed, "visibility_level")
		}
		if !slices.Equal(base.MaskingRules, view.Policy.MaskingRules) {
			changed = append(changed, "masking_rules")
		}
		if base.EvidenceRetention != view.Policy.EvidenceRetention {
			changed = append(changed, "evidence_retention")
		}
		if base.Workflow != view.Policy.Workflow {
			changed = append(changed, "workflow")
		}
		if base.ConfirmationMode != view.Policy.ConfirmationMode {
			changed = append(changed, "confirmation_mode")
		}
		if base.Schedule != view.Policy.Schedule {
			changed = append(changed, "schedule")
		}
		if !slices.Equal(base.InputSources, view.Policy.InputSources) {
			changed = append(changed, "input_sources")
		}
		if !slices.Equal(base.RiskSignalRules, view.Policy.RiskSignalRules) {
			changed = append(changed, "risk_signal_rules")
		}
	}
	high := false
	reasons := []string{}
	for _, field := range changed {
		if slices.Contains([]string{"org_unit_id", "visibility_level", "masking_rules", "evidence_retention", "workflow", "confirmation_mode"}, field) {
			high = true
			reasons = append(reasons, "high_risk_field:"+field)
		}
	}
	level := governance.RiskLow
	if high {
		level = governance.RiskHigh
	}
	return governance.RiskAssessment{RiskLevel: level, RiskReasons: reasons}, changed, view, nil
}

func (s *PolicyService) SubmitReview(ctx context.Context, enterpriseID, policyID, actor, auditRef, operationKey, action string, revision int32, route governance.ReviewRoute, reasons []string) (LifecycleView, error) {
	if err := route.Validate(); err != nil {
		return LifecycleView{}, err
	}
	current, err := s.GetLifecycle(ctx, enterpriseID, policyID)
	if err != nil {
		return LifecycleView{}, err
	}
	if route.ResourceType != governance.ResourceDreamPolicy || route.ResourceID != policyID || route.RequesterUserID != current.RequesterUserID {
		return LifecycleView{}, fmt.Errorf("review route is not bound to Dream policy requester")
	}
	reasonsJSON, _ := json.Marshal(reasons)
	pathJSON, _ := json.Marshal(route.OrgPath)
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	if action != "publish" && action != "disable" {
		return LifecycleView{}, fmt.Errorf("invalid pending action")
	}
	_, err = ls.SubmitDreamPolicyReviewIfRevision(ctx, db.SubmitDreamPolicyReviewIfRevisionParams{PendingAction: action, RiskLevel: string(route.RiskLevel), RiskReasons: reasonsJSON, ReviewMode: string(route.Mode), ReviewerUserID: text(route.ReviewerUserID), ReviewOrgPath: pathJSON, ReviewQueue: text(route.Queue), AuditRefID: text(auditRef), TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision, ActorUserID: actor, OperationKey: operationKey})
	if err != nil {
		current, currentErr := s.GetLifecycle(ctx, enterpriseID, policyID)
		if currentErr == nil && current.ReviewMode == governance.ReviewAdminQueue {
			_, err = ls.RefreshDreamPolicyReviewRoute(ctx, db.RefreshDreamPolicyReviewRouteParams{RiskLevel: string(route.RiskLevel), RiskReasons: reasonsJSON, ReviewMode: string(route.Mode), ReviewerUserID: text(route.ReviewerUserID), ReviewOrgPath: pathJSON, ReviewQueue: text(route.Queue), AuditRefID: text(auditRef), ActorUserID: actor, OperationKey: operationKey, TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision})
		}
		if err != nil {
			return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("submit Dream policy review: %w", err))
		}
	}
	return s.GetLifecycle(ctx, enterpriseID, policyID)
}

func (s *PolicyService) Decide(ctx context.Context, enterpriseID, policyID, actor, auditRef, operationKey, decision string, revision int32) (LifecycleView, error) {
	if decision != "approve" && decision != "reject" {
		return LifecycleView{}, fmt.Errorf("decision must be approve or reject")
	}
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	row, err := ls.DecideDreamPolicyIfRevision(ctx, db.DecideDreamPolicyIfRevisionParams{Decision: decision, AuditRefID: text(auditRef), TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision, ActorUserID: actor, OperationKey: operationKey})
	if err != nil {
		return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("decide Dream policy: %w", err))
	}
	_ = row
	return s.GetLifecycle(ctx, enterpriseID, policyID)
}

func (s *PolicyService) PublishGoverned(ctx context.Context, enterpriseID, policyID, actor, auditRef, operationKey string, revision int32) (LifecycleView, error) {
	view, err := s.GetLifecycle(ctx, enterpriseID, policyID)
	if err != nil {
		return LifecycleView{}, err
	}
	if view.ReviewState != "approved" || view.PendingAction != "publish" || view.Revision != revision || view.PermissionMode != governance.PermissionDirectEdit {
		return LifecycleView{}, fmt.Errorf("Dream policy is not an approved current revision")
	}
	wfStore, ok := s.store.(interface {
		GetWorkflow(context.Context, string) (db.Workflow, error)
		GetWorkflowVersion(context.Context, db.GetWorkflowVersionParams) (db.WorkflowVersion, error)
	})
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store cannot validate published workflows")
	}
	wf, err := wfStore.GetWorkflow(ctx, view.Policy.Workflow.ID)
	if err != nil {
		return LifecycleView{}, err
	}
	wfv, err := wfStore.GetWorkflowVersion(ctx, db.GetWorkflowVersionParams{WorkflowID: view.Policy.Workflow.ID, Version: view.Policy.Workflow.Version})
	if err != nil {
		return LifecycleView{}, err
	}
	if err := validatePublishedDreamWorkflow(enterpriseID, view.Policy, wf, wfv); err != nil {
		return LifecycleView{}, err
	}
	ls := s.store.(policyLifecycleStore)
	published, err := ls.PublishDreamPolicyGoverned(ctx, db.PublishDreamPolicyGovernedParams{AuditRefID: text(auditRef), TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision, ActorUserID: actor, OperationKey: operationKey})
	if err != nil {
		return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("publish governed Dream policy: %w", err))
	}
	view, err = s.GetLifecycle(ctx, enterpriseID, policyID)
	if err != nil {
		return LifecycleView{}, err
	}
	view.Version = published.Version
	return view, nil
}

func (s *PolicyService) Disable(ctx context.Context, enterpriseID, policyID, actor, auditRef, operationKey string, revision int32) (LifecycleView, error) {
	ls, ok := s.store.(policyLifecycleStore)
	if !ok {
		return LifecycleView{}, fmt.Errorf("policy store does not support governed lifecycle")
	}
	row, err := ls.DisableDreamPolicyIfRevision(ctx, db.DisableDreamPolicyIfRevisionParams{AuditRefID: text(auditRef), TargetEnterpriseID: enterpriseID, TargetID: policyID, ExpectedRevision: revision, ActorUserID: actor, OperationKey: operationKey})
	if err != nil {
		return s.reconcileTransition(ctx, enterpriseID, policyID, operationKey, fmt.Errorf("disable Dream policy: %w", err))
	}
	_ = row
	return s.GetLifecycle(ctx, enterpriseID, policyID)
}

func text(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }

// Publish freezes the draft as the next immutable version (admin-confirmed).
func (s *PolicyFixtureService) Publish(ctx context.Context, policyID string) (int32, error) {
	row, err := s.store.GetDreamPolicy(ctx, policyID)
	if err != nil {
		return 0, fmt.Errorf("load policy: %w", err)
	}
	p, err := decodePolicy(row.Draft)
	if err != nil {
		return 0, fmt.Errorf("decode draft: %w", err)
	}
	wfStore, ok := s.store.(interface {
		GetWorkflow(context.Context, string) (db.Workflow, error)
		GetWorkflowVersion(context.Context, db.GetWorkflowVersionParams) (db.WorkflowVersion, error)
	})
	if !ok {
		return 0, fmt.Errorf("policy store cannot validate published workflows")
	}
	wf, err := wfStore.GetWorkflow(ctx, p.Workflow.ID)
	if err != nil {
		return 0, fmt.Errorf("load Dream workflow: %w", err)
	}
	versionRow, err := wfStore.GetWorkflowVersion(ctx, db.GetWorkflowVersionParams{WorkflowID: p.Workflow.ID, Version: p.Workflow.Version})
	if err != nil {
		return 0, fmt.Errorf("load Dream workflow version: %w", err)
	}
	if err := validatePublishedDreamWorkflow(row.EnterpriseID, p, wf, versionRow); err != nil {
		return 0, err
	}
	next := int32(1)
	if latest, err := s.store.GetLatestDreamPolicyVersion(ctx, policyID); err == nil {
		next = latest.Version + 1
	}
	if _, err := s.store.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{
		PolicyID: policyID, Version: next, Definition: row.Draft,
	}); err != nil {
		return 0, fmt.Errorf("publish version: %w", err)
	}
	if _, err := s.store.UpdateDreamPolicyStatus(ctx, db.UpdateDreamPolicyStatusParams{
		ID: policyID, Status: "published",
	}); err != nil {
		return 0, err
	}
	return next, nil
}

func validatePublishedDreamWorkflow(enterpriseID string, p Policy, wf db.Workflow, version db.WorkflowVersion) error {
	if wf.ID != p.Workflow.ID || wf.EnterpriseID != enterpriseID || wf.Kind != string(sdkworkflow.KindDream) {
		return fmt.Errorf("Dream policy workflow must be same-enterprise kind dream")
	}
	if version.WorkflowID != p.Workflow.ID || version.Version != p.Workflow.Version {
		return fmt.Errorf("Dream policy workflow version mismatch")
	}
	var def sdkworkflow.Workflow
	if err := json.Unmarshal(version.Definition, &def); err != nil {
		return fmt.Errorf("decode Dream workflow: %w", err)
	}
	if def.WorkflowID != p.Workflow.ID || def.Version != int(p.Workflow.Version) || def.Kind != sdkworkflow.KindDream {
		return fmt.Errorf("published Dream workflow definition mismatch")
	}
	count := 0
	aggregateID := ""
	traceID := ""
	traceCount := 0
	for _, node := range def.Nodes {
		if node.Type == sdkworkflow.NodeDreamAggregate {
			count++
			aggregateID = node.ID
		}
		if node.Type == sdkworkflow.NodeTraceAppend {
			traceCount++
			traceID = node.ID
		}
	}
	if count != 1 {
		return fmt.Errorf("published Dream workflow requires exactly one dream.aggregate node")
	}
	if traceCount != 1 || !workflowNodeReachable(def.Edges, aggregateID, traceID) {
		return fmt.Errorf("published Dream workflow requires exactly one reachable trace.append after dream.aggregate")
	}
	return nil
}

func workflowNodeReachable(edges []sdkworkflow.Edge, from, target string) bool {
	if from == "" || target == "" || from == target {
		return false
	}
	adjacency := make(map[string][]string)
	for _, edge := range edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if next == target {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// LoadPublished returns the policy definition of the newest published version.
func (s *PolicyService) LoadPublished(ctx context.Context, policyID string) (Policy, int32, error) {
	version, err := s.store.GetLatestDreamPolicyVersion(ctx, policyID)
	if err != nil {
		return Policy{}, 0, fmt.Errorf("policy %s has no published version: %w", policyID, err)
	}
	p, err := decodePolicy(version.Definition)
	if err != nil {
		return Policy{}, 0, err
	}
	return p, version.Version, nil
}

// ListPublished returns the enterprise's published policies with their
// decoded definitions (admin panel listing).
func (s *PolicyService) ListPublished(ctx context.Context, enterpriseID string) ([]PublishedPolicy, error) {
	rows, err := s.store.ListPublishedDreamPolicies(ctx, enterpriseID)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	out := make([]PublishedPolicy, 0, len(rows))
	for _, row := range rows {
		p, err := decodePolicy(row.Draft)
		if err != nil {
			return nil, fmt.Errorf("decode policy %s: %w", row.ID, err)
		}
		out = append(out, PublishedPolicy{ID: row.ID, OrgScope: row.OrgScope, Status: row.Status, Policy: p})
	}
	return out, nil
}

func (s *PolicyService) ListPublishedBounded(ctx context.Context, enterpriseID string, limit int32) ([]PublishedPolicy, error) {
	store, ok := s.store.(interface {
		ListPublishedDreamPoliciesBounded(context.Context, db.ListPublishedDreamPoliciesBoundedParams) ([]db.DreamPolicy, error)
	})
	if !ok {
		return nil, fmt.Errorf("bounded policy listing unavailable")
	}
	rows, err := store.ListPublishedDreamPoliciesBounded(ctx, db.ListPublishedDreamPoliciesBoundedParams{EnterpriseID: enterpriseID, ResultLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]PublishedPolicy, 0, len(rows))
	for _, row := range rows {
		p, err := decodePolicy(row.Draft)
		if err != nil {
			return nil, err
		}
		out = append(out, PublishedPolicy{ID: row.ID, OrgScope: row.OrgScope, Status: row.Status, Policy: p})
	}
	return out, nil
}

// PublishedPolicy pairs a policy row with its decoded definition.
type PublishedPolicy struct {
	ID       string `json:"dream_policy_id"`
	OrgScope string `json:"org_scope"`
	Status   string `json:"status"`
	Policy   Policy `json:"policy"`
}
