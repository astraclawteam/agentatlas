package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type childSummaryResolver struct{ store InputStore }

type childVisibilitySnapshot struct {
	VisibilityLevel sdkdream.VisibilityLevel `json:"visibility_level"`
	OrgUnitIDs      []string                 `json:"org_unit_ids"`
}

func (r childSummaryResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]SourceInput, Coverage, []MissingInput, error) {
	limit := effectiveInputLimit(req.MaxInputs)
	children, err := r.store.ListDreamImmediateChildren(ctx, db.ListDreamImmediateChildrenParams{
		EnterpriseID: req.EnterpriseID, ParentOrgUnitID: req.OrgUnitID, ResultLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list immediate child spaces: %w", err)
	}
	if len(children) > limit {
		return nil, Coverage{}, nil, fmt.Errorf("immediate child spaces exceed bound %d", limit)
	}
	children = scopedChildren(children, req)
	coverage := Coverage{ExpectedChildren: len(children)}
	runs, err := r.store.ListDreamCompletedChildRuns(ctx, db.ListDreamCompletedChildRunsParams{
		EnterpriseID: req.EnterpriseID, ParentOrgUnitID: req.OrgUnitID,
		WindowStart: pgtype.Timestamptz{Time: req.WindowStart, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: req.WindowEnd, Valid: true},
		ResultLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list successful child Dream runs: %w", err)
	}
	if len(runs) > limit {
		return nil, Coverage{}, nil, fmt.Errorf("successful child Dream runs exceed bound %d", limit)
	}
	runs = scopedSuccessfulRuns(runs, req)

	inputs := make([]SourceInput, 0, len(children))
	missing := make([]MissingInput, 0, len(children))
	for _, child := range children {
		run, ok := runForChild(runs, child)
		if !ok {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: sdkdream.MissingNotCompleted})
			continue
		}
		coverage.CompletedChildren++
		summary, ok, err := r.summaryForRun(ctx, req.EnterpriseID, child.ID, run.ID)
		if err != nil {
			return nil, Coverage{}, nil, err
		}
		if !ok {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: sdkdream.MissingNotFound})
			continue
		}
		visibility, valid := childVisibility(run.VisibilitySnapshot)
		if !valid {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: sdkdream.MissingNotAuthorized})
			continue
		}
		pointer := ""
		if summary.EvidencePointerID.Valid {
			pointer = summary.EvidencePointerID.String
		}
		input, reason := makeSourceInput(req, child.ID, sdkdream.SourceChildDreamSummary, summary.ID, child.OrgScope, pointer, run.ID, summary.SummaryText, visibility, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	coverage.InputCount = len(inputs)
	return inputs, coverage, missing, nil
}

func scopedChildren(children []db.ListDreamImmediateChildrenRow, req ResolveRequest) []db.ListDreamImmediateChildrenRow {
	seen := make(map[string]struct{}, len(children))
	result := make([]db.ListDreamImmediateChildrenRow, 0, len(children))
	for _, child := range children {
		if child.EnterpriseID != req.EnterpriseID || child.ID == "" || child.OrgScope == "" ||
			!parentIdentityMatches(child.ParentScopeKind, child.ParentScopeID, child.ParentOrgScope, req.OrgUnitID) ||
			(req.SpaceID != "" && child.ParentSpaceID != req.SpaceID) {
			continue
		}
		if _, ok := parseScopeRef(child.OrgScope); !ok {
			continue
		}
		if !scopeKindMatches(child.Kind, child.OrgScope) && child.Kind != "" {
			continue
		}
		if _, ok := seen[child.ID]; ok {
			continue
		}
		seen[child.ID] = struct{}{}
		result = append(result, child)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].OrgScope != result[j].OrgScope {
			return result[i].OrgScope < result[j].OrgScope
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func scopedSuccessfulRuns(runs []db.ListDreamCompletedChildRunsRow, req ResolveRequest) []db.ListDreamCompletedChildRunsRow {
	result := make([]db.ListDreamCompletedChildRunsRow, 0, len(runs))
	for _, run := range runs {
		if run.EnterpriseID != req.EnterpriseID || run.Status != "succeeded" || run.ChildSpaceID == "" || run.ChildOrgScope == "" ||
			!parentIdentityMatches(run.ParentScopeKind, run.ParentScopeID, run.ParentOrgScope, req.OrgUnitID) ||
			(req.SpaceID != "" && run.ParentSpaceID != req.SpaceID) {
			continue
		}
		if !run.WindowStart.Valid || !run.WindowEnd.Valid || !run.WindowStart.Time.Equal(req.WindowStart) || !run.WindowEnd.Time.Equal(req.WindowEnd) {
			continue
		}
		if !sameOrgUnit(run.ChildOrgScope, run.OrgUnitID) {
			continue
		}
		result = append(result, run)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ChildSpaceID != result[j].ChildSpaceID {
			return result[i].ChildSpaceID < result[j].ChildSpaceID
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func runForChild(runs []db.ListDreamCompletedChildRunsRow, child db.ListDreamImmediateChildrenRow) (db.ListDreamCompletedChildRunsRow, bool) {
	for _, run := range runs {
		if run.ChildSpaceID == child.ID && sameOrgUnit(run.ChildOrgScope, child.OrgScope) && sameOrgUnit(run.OrgUnitID, child.OrgScope) {
			return run, true
		}
	}
	return db.ListDreamCompletedChildRunsRow{}, false
}

func (r childSummaryResolver) summaryForRun(ctx context.Context, enterpriseID, spaceID, runID string) (db.DreamSummary, bool, error) {
	for _, layer := range []string{"retrieval", "display"} {
		summary, err := r.store.GetDreamSummaryForRunLayer(ctx, db.GetDreamSummaryForRunLayerParams{
			EnterpriseID: enterpriseID, RunID: runID, SpaceID: spaceID, Layer: layer,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return db.DreamSummary{}, false, fmt.Errorf("get child %s summary: %w", layer, err)
		}
		if summary.EnterpriseID != enterpriseID || summary.SpaceID != spaceID || summary.RunID != runID || summary.Layer != layer || summary.SummaryText == "" {
			return db.DreamSummary{}, false, fmt.Errorf("child %s summary returned invalid provenance", layer)
		}
		return summary, true, nil
	}
	return db.DreamSummary{}, false, nil
}

func childVisibility(raw []byte) ([]string, bool) {
	var snapshot childVisibilitySnapshot
	if len(raw) == 0 || json.Unmarshal(raw, &snapshot) != nil || !visibilityLevelToken(string(snapshot.VisibilityLevel)) || len(snapshot.OrgUnitIDs) == 0 {
		return nil, false
	}
	visibility := make([]string, 0, len(snapshot.OrgUnitIDs)+1)
	for _, orgUnitID := range snapshot.OrgUnitIDs {
		if visibilityLevelToken(orgUnitID) {
			return nil, false
		}
		if _, ok := parseScopeRef(orgUnitID); !ok {
			return nil, false
		}
		visibility = append(visibility, orgUnitID)
	}
	visibility = append(visibility, string(snapshot.VisibilityLevel))
	visibility = normalizeStrings(visibility, maxVisibilityEntries+1)
	return visibility, len(visibility) >= 2 && len(visibility) <= maxVisibilityEntries
}

func parentIdentityMatches(kind, id, orgScope, requested string) bool {
	parent, ok := parseScopeRef(orgScope)
	if !ok || parent.kind == "" || parent.kind != kind || parent.id != id {
		return false
	}
	return sameOrgUnit(orgScope, requested)
}
