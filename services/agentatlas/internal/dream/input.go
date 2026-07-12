package dream

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

const (
	defaultMaxResolvedInputs = 500
	maxResolvedInputs        = 1000
	maxResolvedTextRunes     = 4000
	maxVisibilityEntries     = 64
)

type ResolvedInput struct {
	SourceType        sdkdream.Source
	SourceID          string
	OrgUnitID         string
	EvidencePointerID string
	SanitizedText     string
	Visibility        []string
	ParentRunID       string
}

type Coverage struct {
	ExpectedChildren  int
	CompletedChildren int
	InputCount        int
}

type MissingInput = sdkdream.MissingInput

type ResolveRequest struct {
	EnterpriseID string
	OrgUnitID    string
	WindowStart  time.Time
	WindowEnd    time.Time
	Sources      []sdkdream.Source
	MaskingRules []string
	Visibility   []string
	MaxInputs    int
}

type Request = ResolveRequest

type InputResolver interface {
	Resolve(context.Context, ResolveRequest) ([]ResolvedInput, Coverage, []MissingInput, error)
}

// SourceResolver is the extension point for typed Dream sources. Implementations
// return only bounded records for the request; Resolver reapplies masking,
// normalization, deduplication, and the final output bound.
type SourceResolver interface {
	ResolveSource(context.Context, ResolveRequest, *Masker) ([]ResolvedInput, Coverage, []MissingInput, error)
}

// InputStore is deliberately composed only of generated PostgreSQL query
// contracts. Resolver implementations must not bypass these enterprise/window
// boundaries with alternate persistence paths.
type InputStore interface {
	GetKnowledgeSpaceByScope(context.Context, db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error)
	ListSpaceMembers(context.Context, string) ([]db.SpaceMembershipCache, error)
	ListWorkBriefsForWindow(context.Context, db.ListWorkBriefsForWindowParams) ([]db.WorkBrief, error)
	ListChildSpaces(context.Context, db.ListChildSpacesParams) ([]db.KnowledgeSpace, error)
	ListCompletedChildDreamRuns(context.Context, db.ListCompletedChildDreamRunsParams) ([]db.DreamRun, error)
	ListDreamSummariesBySpace(context.Context, db.ListDreamSummariesBySpaceParams) ([]db.DreamSummary, error)
	ListTimelineNodes(context.Context, db.ListTimelineNodesParams) ([]db.TimelineNode, error)
}

type Resolver struct {
	store     InputStore
	resolvers map[sdkdream.Source]SourceResolver
}

func NewInputResolver(store InputStore) *Resolver {
	r := &Resolver{store: store, resolvers: make(map[sdkdream.Source]SourceResolver)}
	r.Register(sdkdream.SourceWorkBrief, workBriefResolver{store: store})
	r.Register(sdkdream.SourceChildDreamSummary, childSummaryResolver{store: store})
	for _, source := range []sdkdream.Source{
		sdkdream.SourceProjectRecord,
		sdkdream.SourceSOPUpdate,
		sdkdream.SourceAgentAnswer,
		sdkdream.SourceExternalEvidence,
		sdkdream.SourceCompletedTask,
		sdkdream.SourceRiskEvent,
	} {
		r.Register(source, timelineResolver{store: store, source: source})
	}
	return r
}

func (r *Resolver) Register(source sdkdream.Source, resolver SourceResolver) {
	if source == "" || resolver == nil {
		return
	}
	r.resolvers[source] = resolver
}

func (r *Resolver) Resolve(ctx context.Context, req ResolveRequest) ([]ResolvedInput, Coverage, []MissingInput, error) {
	if r == nil || r.store == nil {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input resolver requires a store")
	}
	if !req.WindowStart.IsZero() && !req.WindowEnd.IsZero() && req.WindowEnd.Before(req.WindowStart) {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input window ends before it starts")
	}
	masker, err := NewMasker(req.MaskingRules)
	if err != nil {
		return nil, Coverage{}, nil, err
	}
	sources, err := r.resolveSources(ctx, req)
	if err != nil {
		return nil, Coverage{}, nil, err
	}

	var coverage Coverage
	var inputs []ResolvedInput
	var missing []MissingInput
	for _, source := range sources {
		resolver, ok := r.resolvers[source]
		if !ok {
			return nil, Coverage{}, nil, fmt.Errorf("unsupported Dream input source %q", source)
		}
		resolved, sourceCoverage, sourceMissing, err := resolver.ResolveSource(ctx, req, masker)
		if err != nil {
			return nil, Coverage{}, nil, fmt.Errorf("resolve %s: %w", source, err)
		}
		coverage.ExpectedChildren += sourceCoverage.ExpectedChildren
		coverage.CompletedChildren += sourceCoverage.CompletedChildren
		for _, input := range resolved {
			input.SourceType = source
			visibility, ok := intersectVisibility(input.Visibility, req.Visibility)
			if !ok {
				sourceMissing = append(sourceMissing, MissingInput{SourceType: source, SourceID: input.SourceID, Reason: sdkdream.MissingNotAuthorized})
				continue
			}
			input.Visibility = visibility
			input.SanitizedText = truncateRunes(strings.TrimSpace(masker.Apply(input.SanitizedText)), maxResolvedTextRunes)
			inputs = append(inputs, input)
		}
		missing = append(missing, sourceMissing...)
	}

	inputs = normalizeInputs(inputs, masker)
	limit := req.MaxInputs
	if limit <= 0 {
		limit = defaultMaxResolvedInputs
	}
	if limit > maxResolvedInputs {
		limit = maxResolvedInputs
	}
	if len(inputs) > limit {
		inputs = inputs[:limit]
	}
	missing = normalizeMissing(missing)
	coverage.InputCount = len(inputs)
	return inputs, coverage, missing, nil
}

func (r *Resolver) resolveSources(ctx context.Context, req ResolveRequest) ([]sdkdream.Source, error) {
	if len(req.Sources) > 0 {
		return uniqueSources(req.Sources), nil
	}
	space, err := r.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     req.OrgUnitID,
	})
	if err != nil {
		return nil, fmt.Errorf("load Dream org scope %s: %w", req.OrgUnitID, err)
	}
	if space.EnterpriseID != req.EnterpriseID || !sameOrgUnit(space.OrgScope, req.OrgUnitID) {
		return nil, fmt.Errorf("Dream org scope %s is outside enterprise %s", req.OrgUnitID, req.EnterpriseID)
	}
	if directBriefScope(space.Kind) {
		return []sdkdream.Source{sdkdream.SourceWorkBrief}, nil
	}
	return []sdkdream.Source{sdkdream.SourceChildDreamSummary}, nil
}

func uniqueSources(sources []sdkdream.Source) []sdkdream.Source {
	seen := make(map[sdkdream.Source]struct{}, len(sources))
	result := make([]sdkdream.Source, 0, len(sources))
	for _, source := range sources {
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		result = append(result, source)
	}
	return result
}

func makeResolvedInput(req ResolveRequest, source sdkdream.Source, sourceID, orgUnitID, evidencePointerID, parentRunID, rawText string, sourceVisibility []string, masker *Masker) (ResolvedInput, sdkdream.MissingReason) {
	visibility, ok := intersectVisibility(sourceVisibility, req.Visibility)
	if !ok {
		return ResolvedInput{}, sdkdream.MissingNotAuthorized
	}
	text := strings.TrimSpace(masker.Apply(rawText))
	if text == "" {
		return ResolvedInput{}, sdkdream.MissingMasked
	}
	return ResolvedInput{
		SourceType: source, SourceID: sourceID, OrgUnitID: orgUnitID,
		EvidencePointerID: evidencePointerID, SanitizedText: truncateRunes(text, maxResolvedTextRunes),
		Visibility: visibility, ParentRunID: parentRunID,
	}, ""
}

func intersectVisibility(source, requested []string) ([]string, bool) {
	source = normalizeStrings(source, maxVisibilityEntries)
	requested = normalizeStrings(requested, maxVisibilityEntries)
	if len(source) == 0 {
		return requested, true
	}
	if len(requested) == 0 {
		return source, true
	}
	allowed := make(map[string]struct{}, len(source))
	for _, item := range source {
		allowed[item] = struct{}{}
	}
	result := make([]string, 0, len(requested))
	var sourceHasLevel, requestedHasLevel, matchedLevel bool
	var sourceHasScope, requestedHasScope, matchedScope bool
	for _, item := range source {
		if visibilityLevelToken(item) {
			sourceHasLevel = true
		} else {
			sourceHasScope = true
		}
	}
	for _, item := range requested {
		isLevel := visibilityLevelToken(item)
		if isLevel {
			requestedHasLevel = true
		} else {
			requestedHasScope = true
		}
		if _, ok := allowed[item]; ok {
			result = append(result, item)
			if isLevel {
				matchedLevel = true
			} else {
				matchedScope = true
			}
		}
	}
	if sourceHasLevel && requestedHasLevel && !matchedLevel {
		return nil, false
	}
	if sourceHasScope && requestedHasScope && !matchedScope {
		return nil, false
	}
	return result, len(result) > 0
}

func visibilityLevelToken(value string) bool {
	switch sdkdream.VisibilityLevel(value) {
	case sdkdream.VisibilityMembers, sdkdream.VisibilityManagers, sdkdream.VisibilityCompanySanitized:
		return true
	default:
		return false
	}
}

func normalizeStrings(values []string, limit int) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func normalizeInputs(inputs []ResolvedInput, masker *Masker) []ResolvedInput {
	for i := range inputs {
		inputs[i].SanitizedText = truncateRunes(strings.TrimSpace(masker.Apply(inputs[i].SanitizedText)), maxResolvedTextRunes)
		inputs[i].Visibility = normalizeStrings(inputs[i].Visibility, maxVisibilityEntries)
	}
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].SourceType != inputs[j].SourceType {
			return inputs[i].SourceType < inputs[j].SourceType
		}
		if inputs[i].SourceID != inputs[j].SourceID {
			return inputs[i].SourceID < inputs[j].SourceID
		}
		return inputs[i].ParentRunID < inputs[j].ParentRunID
	})
	seen := make(map[string]struct{}, len(inputs))
	result := make([]ResolvedInput, 0, len(inputs))
	for _, input := range inputs {
		if input.SourceID == "" || input.SanitizedText == "" {
			continue
		}
		key := string(input.SourceType) + "\x00" + input.SourceID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, input)
	}
	return result
}

func normalizeMissing(missing []MissingInput) []MissingInput {
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].SourceType != missing[j].SourceType {
			return missing[i].SourceType < missing[j].SourceType
		}
		if missing[i].SourceID != missing[j].SourceID {
			return missing[i].SourceID < missing[j].SourceID
		}
		return missing[i].Reason < missing[j].Reason
	})
	seen := make(map[string]struct{}, len(missing))
	result := make([]MissingInput, 0, len(missing))
	for _, item := range missing {
		key := string(item.SourceType) + "\x00" + item.SourceID + "\x00" + string(item.Reason)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
		if len(result) == maxResolvedInputs {
			break
		}
	}
	return result
}

func directBriefScope(kind string) bool {
	return kind == "employee" || kind == "project_group"
}

func sameOrgUnit(stored, requested string) bool {
	return stored == requested || strings.HasSuffix(stored, ":"+requested) || strings.HasSuffix(requested, ":"+stored)
}
