package dream

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
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

// SourceInput is the internal, provenance-bearing value exchanged across a
// pluggable source boundary. MaskedText can only be created by the exact
// request Masker; Resolver rejects values without that opaque provenance.
type SourceInput struct {
	SourceType        sdkdream.Source
	SourceID          string
	OrgUnitID         string
	EvidencePointerID string
	MaskedText        MaskedText
	Visibility        []string
	ParentRunID       string
	EnterpriseID      string
	SpaceID           string
	WindowStart       time.Time
	WindowEnd         time.Time
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
	Timezone     string
	Sources      []sdkdream.Source
	MaskingRules []string
	Visibility   []string
	MaxInputs    int
	SpaceID      string
}

type Request = ResolveRequest

type InputResolver interface {
	Resolve(context.Context, ResolveRequest) ([]ResolvedInput, Coverage, []MissingInput, error)
}

// SourceResolver is the extension point for typed Dream sources. Implementations
// must return bounded, already-masked records; Resolver rejects violations and
// applies only visibility convergence, deduplication, and final ordering.
type SourceResolver interface {
	ResolveSource(context.Context, ResolveRequest, *Masker) ([]SourceInput, Coverage, []MissingInput, error)
}

// InputStore is deliberately composed only of generated PostgreSQL query
// contracts. Resolver implementations must not bypass these enterprise/window
// boundaries with alternate persistence paths.
type InputStore interface {
	GetKnowledgeSpaceByScope(context.Context, db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error)
	ListDreamSpaceMembers(context.Context, db.ListDreamSpaceMembersParams) ([]db.SpaceMembershipCache, error)
	ListDreamWorkBriefsForWindow(context.Context, db.ListDreamWorkBriefsForWindowParams) ([]db.WorkBrief, error)
	ListDreamImmediateChildren(context.Context, db.ListDreamImmediateChildrenParams) ([]db.ListDreamImmediateChildrenRow, error)
	ListDreamCompletedChildRuns(context.Context, db.ListDreamCompletedChildRunsParams) ([]db.ListDreamCompletedChildRunsRow, error)
	GetDreamSummaryForRunLayer(context.Context, db.GetDreamSummaryForRunLayerParams) (db.DreamSummary, error)
	ListDreamTimelineNodes(context.Context, db.ListDreamTimelineNodesParams) ([]db.TimelineNode, error)
}

type Resolver struct {
	mu        sync.RWMutex
	store     InputStore
	resolvers map[sdkdream.Source]SourceResolver
	builtins  map[sdkdream.Source]struct{}
}

func NewInputResolver(store InputStore) *Resolver {
	r := &Resolver{store: store, resolvers: make(map[sdkdream.Source]SourceResolver), builtins: make(map[sdkdream.Source]struct{})}
	r.addBuiltin(sdkdream.SourceWorkBrief, workBriefResolver{store: store})
	r.addBuiltin(sdkdream.SourceChildDreamSummary, childSummaryResolver{store: store})
	for _, source := range []sdkdream.Source{
		sdkdream.SourceProjectRecord,
		sdkdream.SourceSOPUpdate,
		sdkdream.SourceAgentAnswer,
		sdkdream.SourceExternalEvidence,
		sdkdream.SourceCompletedTask,
		sdkdream.SourceRiskEvent,
	} {
		r.addBuiltin(source, timelineResolver{store: store, source: source})
	}
	return r
}

func (r *Resolver) addBuiltin(source sdkdream.Source, resolver SourceResolver) {
	r.resolvers[source] = resolver
	r.builtins[source] = struct{}{}
}

func (r *Resolver) Register(source sdkdream.Source, resolver SourceResolver) error {
	if source == "" || resolver == nil {
		return fmt.Errorf("Dream source and resolver are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, protected := r.builtins[source]; protected {
		return fmt.Errorf("built-in Dream input source %q cannot be replaced", source)
	}
	r.resolvers[source] = resolver
	return nil
}

func (r *Resolver) Resolve(ctx context.Context, req ResolveRequest) ([]ResolvedInput, Coverage, []MissingInput, error) {
	if r == nil || r.store == nil {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input resolver requires a store")
	}
	if req.EnterpriseID == "" || req.OrgUnitID == "" {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input enterprise and org unit are required")
	}
	if req.WindowStart.IsZero() || req.WindowEnd.IsZero() || !req.WindowEnd.After(req.WindowStart) {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input window must have nonempty increasing endpoints")
	}
	if _, err := dreamLocation(req.Timezone); err != nil {
		return nil, Coverage{}, nil, err
	}
	if len(req.Sources) > maxResolvedInputs {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input source count exceeds bound %d", maxResolvedInputs)
	}
	if _, ok := parseScopeRef(req.OrgUnitID); !ok {
		return nil, Coverage{}, nil, fmt.Errorf("invalid Dream org unit %q", req.OrgUnitID)
	}
	requestVisibility := normalizeStrings(req.Visibility, maxVisibilityEntries+1)
	if len(requestVisibility) == 0 || len(requestVisibility) > maxVisibilityEntries {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input visibility must be nonempty")
	}
	masker, err := NewMasker(req.MaskingRules)
	if err != nil {
		return nil, Coverage{}, nil, err
	}
	sources, err := r.resolveSources(ctx, req)
	if err != nil {
		return nil, Coverage{}, nil, err
	}
	if len(sources) > maxResolvedInputs {
		return nil, Coverage{}, nil, fmt.Errorf("Dream input source count exceeds bound %d", maxResolvedInputs)
	}

	var coverage Coverage
	var inputs []ResolvedInput
	var missing []MissingInput
	limit := effectiveInputLimit(req.MaxInputs)
	for _, source := range sources {
		resolver, builtin, ok := r.sourceResolver(source)
		if !ok {
			return nil, Coverage{}, nil, fmt.Errorf("unsupported Dream input source %q", source)
		}
		resolved, sourceCoverage, sourceMissing, err := resolver.ResolveSource(ctx, req, masker)
		if err != nil {
			return nil, Coverage{}, nil, fmt.Errorf("resolve %s: %w", source, err)
		}
		if err := validateSourceMetadata(source, resolved, sourceCoverage, sourceMissing, req, builtin, masker); err != nil {
			return nil, Coverage{}, nil, fmt.Errorf("resolve %s: %w", source, err)
		}
		if len(inputs)+len(resolved) > limit {
			return nil, Coverage{}, nil, fmt.Errorf("Dream input sources returned aggregate inputs above bound %d", limit)
		}
		if len(missing)+len(sourceMissing) > maxResolvedInputs {
			return nil, Coverage{}, nil, fmt.Errorf("Dream input sources returned aggregate missing inputs above bound %d", maxResolvedInputs)
		}
		if coverage.ExpectedChildren > maxResolvedInputs-sourceCoverage.ExpectedChildren || coverage.CompletedChildren > maxResolvedInputs-sourceCoverage.CompletedChildren {
			return nil, Coverage{}, nil, fmt.Errorf("Dream input sources returned aggregate coverage above bound %d", maxResolvedInputs)
		}
		coverage.ExpectedChildren += sourceCoverage.ExpectedChildren
		coverage.CompletedChildren += sourceCoverage.CompletedChildren
		for _, input := range resolved {
			visibility, ok := intersectVisibility(input.Visibility, req.Visibility)
			if !ok {
				if len(missing)+len(sourceMissing) >= maxResolvedInputs {
					return nil, Coverage{}, nil, fmt.Errorf("Dream input sources returned aggregate missing inputs above bound %d", maxResolvedInputs)
				}
				sourceMissing = append(sourceMissing, MissingInput{SourceType: source, SourceID: input.SourceID, Reason: sdkdream.MissingNotAuthorized})
				continue
			}
			input.Visibility = visibility
			text, ok := masker.resolve(input.MaskedText)
			if !ok {
				return nil, Coverage{}, nil, fmt.Errorf("resolve %s: source %s returned text without masking provenance", source, input.SourceID)
			}
			inputs = append(inputs, ResolvedInput{
				SourceType: input.SourceType, SourceID: input.SourceID, OrgUnitID: input.OrgUnitID,
				EvidencePointerID: input.EvidencePointerID, SanitizedText: text,
				Visibility: input.Visibility, ParentRunID: input.ParentRunID,
			})
		}
		missing = append(missing, sourceMissing...)
	}

	inputs = normalizeInputs(inputs)
	if len(inputs) > limit {
		inputs = inputs[:limit]
	}
	missing = normalizeMissing(missing)
	coverage.InputCount = len(inputs)
	return inputs, coverage, missing, nil
}

func (r *Resolver) sourceResolver(source sdkdream.Source) (SourceResolver, bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	resolver, ok := r.resolvers[source]
	_, builtin := r.builtins[source]
	return resolver, builtin, ok
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

func validateSourceMetadata(source sdkdream.Source, inputs []SourceInput, coverage Coverage, missing []MissingInput, req ResolveRequest, builtin bool, masker *Masker) error {
	limit := effectiveInputLimit(req.MaxInputs)
	if len(inputs) > limit {
		return fmt.Errorf("source returned %d inputs above bound %d", len(inputs), limit)
	}
	if len(missing) > limit {
		return fmt.Errorf("source returned %d missing inputs above bound %d", len(missing), limit)
	}
	if coverage.ExpectedChildren < 0 || coverage.CompletedChildren < 0 || coverage.InputCount < 0 || coverage.CompletedChildren > coverage.ExpectedChildren || coverage.ExpectedChildren > maxResolvedInputs || coverage.InputCount > maxResolvedInputs {
		return fmt.Errorf("invalid source coverage %+v", coverage)
	}
	if coverage.InputCount != len(inputs) {
		return fmt.Errorf("source coverage input count %d does not match %d inputs", coverage.InputCount, len(inputs))
	}
	if source != sdkdream.SourceChildDreamSummary && (coverage.ExpectedChildren != 0 || coverage.CompletedChildren != 0) {
		return fmt.Errorf("source %s does not own child coverage", source)
	}
	for _, input := range inputs {
		if input.SourceType != source || input.SourceID == "" || input.EnterpriseID != req.EnterpriseID || !input.WindowStart.Equal(req.WindowStart) || !input.WindowEnd.Equal(req.WindowEnd) {
			return fmt.Errorf("source %s returned invalid provenance for %q", source, input.SourceID)
		}
		if source == sdkdream.SourceChildDreamSummary {
			if input.SpaceID == "" || input.ParentRunID == "" || !builtin {
				return fmt.Errorf("child source returned incomplete provenance for %q", input.SourceID)
			}
		} else {
			if !sameOrgUnit(input.OrgUnitID, req.OrgUnitID) {
				return fmt.Errorf("source %s returned invalid org provenance for %q", source, input.SourceID)
			}
			if !builtin && (req.SpaceID == "" || input.SpaceID != req.SpaceID) {
				return fmt.Errorf("source %s returned invalid space provenance for %q", source, input.SourceID)
			}
		}
		if _, ok := masker.resolve(input.MaskedText); !ok {
			return fmt.Errorf("source %s returned invalid sanitized text for %q", source, input.SourceID)
		}
		if visibility := normalizeStrings(input.Visibility, maxVisibilityEntries+1); len(visibility) == 0 || len(visibility) > maxVisibilityEntries {
			return fmt.Errorf("source %s returned invalid visibility for %q", source, input.SourceID)
		}
	}
	for _, item := range missing {
		if item.SourceType != source || item.SourceID == "" || !validMissingReason(item.Reason) {
			return fmt.Errorf("source %s returned invalid missing input %+v", source, item)
		}
	}
	return nil
}

func validMissingReason(reason sdkdream.MissingReason) bool {
	switch reason {
	case sdkdream.MissingNotFound, sdkdream.MissingNotCompleted, sdkdream.MissingNotAuthorized, sdkdream.MissingFailed, sdkdream.MissingMasked:
		return true
	default:
		return false
	}
}

func effectiveInputLimit(requested int) int {
	if requested <= 0 {
		return defaultMaxResolvedInputs
	}
	if requested > maxResolvedInputs {
		return maxResolvedInputs
	}
	return requested
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

func makeSourceInput(req ResolveRequest, spaceID string, source sdkdream.Source, sourceID, orgUnitID, evidencePointerID, parentRunID, rawText string, sourceVisibility []string, masker *Masker) (SourceInput, sdkdream.MissingReason) {
	visibility, ok := intersectVisibility(sourceVisibility, req.Visibility)
	if !ok {
		return SourceInput{}, sdkdream.MissingNotAuthorized
	}
	masked := masker.Sanitize(rawText)
	if _, ok := masker.resolve(masked); !ok {
		return SourceInput{}, sdkdream.MissingMasked
	}
	return SourceInput{
		SourceType: source, SourceID: sourceID, OrgUnitID: orgUnitID,
		EvidencePointerID: evidencePointerID, MaskedText: masked,
		Visibility: visibility, ParentRunID: parentRunID,
		EnterpriseID: req.EnterpriseID, SpaceID: spaceID, WindowStart: req.WindowStart, WindowEnd: req.WindowEnd,
	}, ""
}

func intersectVisibility(source, requested []string) ([]string, bool) {
	source = normalizeStrings(source, maxVisibilityEntries)
	requested = normalizeStrings(requested, maxVisibilityEntries)
	if len(source) == 0 || len(requested) == 0 {
		return nil, false
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

func normalizeInputs(inputs []ResolvedInput) []ResolvedInput {
	for i := range inputs {
		inputs[i].Visibility = normalizeStrings(inputs[i].Visibility, maxVisibilityEntries)
	}
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].SourceType != inputs[j].SourceType {
			return inputs[i].SourceType < inputs[j].SourceType
		}
		if inputs[i].SourceID != inputs[j].SourceID {
			return inputs[i].SourceID < inputs[j].SourceID
		}
		if inputs[i].ParentRunID != inputs[j].ParentRunID {
			return inputs[i].ParentRunID < inputs[j].ParentRunID
		}
		if inputs[i].OrgUnitID != inputs[j].OrgUnitID {
			return inputs[i].OrgUnitID < inputs[j].OrgUnitID
		}
		if inputs[i].EvidencePointerID != inputs[j].EvidencePointerID {
			return inputs[i].EvidencePointerID < inputs[j].EvidencePointerID
		}
		if inputs[i].SanitizedText != inputs[j].SanitizedText {
			return inputs[i].SanitizedText < inputs[j].SanitizedText
		}
		return strings.Join(inputs[i].Visibility, "\x00") < strings.Join(inputs[j].Visibility, "\x00")
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

func scopeKindMatches(kind, scope string) bool {
	parsed, ok := parseScopeRef(scope)
	return ok && (parsed.kind == "" || parsed.kind == kind)
}

func sameOrgUnit(stored, requested string) bool {
	left, leftOK := parseScopeRef(stored)
	right, rightOK := parseScopeRef(requested)
	if !leftOK || !rightOK {
		return false
	}
	if left.kind != "" && right.kind != "" {
		return left.kind == right.kind && left.id == right.id
	}
	return left.id == right.id
}

type scopeRef struct{ kind, id string }

func parseScopeRef(value string) (scopeRef, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return scopeRef{}, false
	}
	parts := strings.Split(value, ":")
	if len(parts) == 1 {
		return scopeRef{id: value}, true
	}
	if len(parts) != 2 || parts[1] == "" {
		return scopeRef{}, false
	}
	switch parts[0] {
	case "employee", "project_group", "department", "business_unit", "company":
		return scopeRef{kind: parts[0], id: parts[1]}, true
	default:
		return scopeRef{}, false
	}
}
