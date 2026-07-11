// Package dream implements the organizational memory synthesis loop: dream
// policies describe how an org scope is periodically summarized; dream jobs
// aggregate work briefs into layered summaries (display / retrieval / sealed
// pointer) under visibility and masking rules.
package dream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/robfig/cron/v3"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
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

// CreateDraft validates and stores a draft policy.
func (s *PolicyService) CreateDraft(ctx context.Context, enterpriseID string, p Policy) (string, error) {
	return s.CreateDraftWithID(ctx, enterpriseID, NewPolicyID(), p)
}

func (s *PolicyService) CreateDraftWithID(ctx context.Context, enterpriseID, policyID string, p Policy) (string, error) {
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

// Publish freezes the draft as the next immutable version (admin-confirmed).
func (s *PolicyService) Publish(ctx context.Context, policyID string) (int32, error) {
	row, err := s.store.GetDreamPolicy(ctx, policyID)
	if err != nil {
		return 0, fmt.Errorf("load policy: %w", err)
	}
	if _, err := decodePolicy(row.Draft); err != nil {
		return 0, fmt.Errorf("decode draft: %w", err)
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

// PublishedPolicy pairs a policy row with its decoded definition.
type PublishedPolicy struct {
	ID       string `json:"dream_policy_id"`
	OrgScope string `json:"org_scope"`
	Status   string `json:"status"`
	Policy   Policy `json:"policy"`
}
