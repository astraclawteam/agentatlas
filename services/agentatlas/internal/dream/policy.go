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

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Policy is the dream_policies definition payload (versioned on publish).
type Policy struct {
	OrgScope        string   `json:"org_scope"`
	Schedule        string   `json:"schedule"` // cron, enterprise timezone
	InputSources    []string `json:"input_sources"`
	VisibilityLevel string   `json:"visibility_level"` // members | managers | company_sanitized
	MaskingRules    []string `json:"masking_rules,omitempty"`
	RiskSignalRules []string `json:"risk_signal_rules,omitempty"`
	EvidenceRetention string `json:"evidence_retention"` // pointer_only | pointer_plus_display_summary
	OutputSpaceID   string   `json:"output_space_id"`
	MaxAttempts     int      `json:"max_attempts,omitempty"`
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Validate fails loud on anything that would make a run unsafe or undefined.
func (p Policy) Validate() error {
	if p.OrgScope == "" {
		return fmt.Errorf("dream policy: org_scope is required")
	}
	if p.OutputSpaceID == "" {
		return fmt.Errorf("dream policy: output_space_id is required")
	}
	if _, err := cronParser.Parse(p.Schedule); err != nil {
		return fmt.Errorf("dream policy: bad schedule %q: %w", p.Schedule, err)
	}
	switch p.VisibilityLevel {
	case "members", "managers", "company_sanitized":
	default:
		return fmt.Errorf("dream policy: unknown visibility level %q", p.VisibilityLevel)
	}
	switch p.EvidenceRetention {
	case "pointer_only", "pointer_plus_display_summary":
	default:
		return fmt.Errorf("dream policy: unknown evidence retention %q", p.EvidenceRetention)
	}
	if len(p.InputSources) == 0 {
		return fmt.Errorf("dream policy: at least one input source required")
	}
	for _, src := range p.InputSources {
		switch src {
		case "work_briefs", "project_records", "sop_updates", "agent_answers", "external_evidence":
		default:
			return fmt.Errorf("dream policy: unknown input source %q", src)
		}
	}
	for _, rule := range append(append([]string{}, p.MaskingRules...), p.RiskSignalRules...) {
		if _, err := regexp.Compile(rule); err != nil {
			return fmt.Errorf("dream policy: bad rule %q: %w", rule, err)
		}
	}
	return nil
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

// CreateDraft validates and stores a draft policy.
func (s *PolicyService) CreateDraft(ctx context.Context, enterpriseID string, p Policy) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	row, err := s.store.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{
		ID: newID("pol"), EnterpriseID: enterpriseID, OrgScope: p.OrgScope,
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
	var p Policy
	if err := json.Unmarshal(row.Draft, &p); err != nil {
		return 0, fmt.Errorf("decode draft: %w", err)
	}
	if err := p.Validate(); err != nil {
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

// LoadPublished returns the policy definition of the newest published version.
func (s *PolicyService) LoadPublished(ctx context.Context, policyID string) (Policy, int32, error) {
	version, err := s.store.GetLatestDreamPolicyVersion(ctx, policyID)
	if err != nil {
		return Policy{}, 0, fmt.Errorf("policy %s has no published version: %w", policyID, err)
	}
	var p Policy
	if err := json.Unmarshal(version.Definition, &p); err != nil {
		return Policy{}, 0, err
	}
	return p, version.Version, nil
}
