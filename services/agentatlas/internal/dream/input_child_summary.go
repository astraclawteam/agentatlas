package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

type childSummaryResolver struct{ store InputStore }

type childVisibilitySnapshot struct {
	VisibilityLevel string   `json:"visibility_level"`
	OrgUnitIDs      []string `json:"org_unit_ids"`
}

func (r childSummaryResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]ResolvedInput, Coverage, []MissingInput, error) {
	children, err := r.store.ListChildSpaces(ctx, db.ListChildSpacesParams{
		EnterpriseID: req.EnterpriseID, ParentOrgUnitID: req.OrgUnitID,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list immediate child spaces: %w", err)
	}
	children = scopedChildren(children, req)
	coverage := Coverage{ExpectedChildren: len(children)}
	runs, err := r.store.ListCompletedChildDreamRuns(ctx, db.ListCompletedChildDreamRunsParams{
		EnterpriseID: req.EnterpriseID, ParentOrgUnitID: req.OrgUnitID,
		WindowStart: pgtype.Timestamptz{Time: req.WindowStart, Valid: !req.WindowStart.IsZero()},
		WindowEnd:   pgtype.Timestamptz{Time: req.WindowEnd, Valid: !req.WindowEnd.IsZero()},
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list successful child Dream runs: %w", err)
	}
	runs = scopedSuccessfulRuns(runs, req)

	inputs := make([]ResolvedInput, 0, len(children))
	missing := make([]MissingInput, 0)
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
		visibility := childVisibility(run.VisibilitySnapshot, child.OrgScope)
		pointer := ""
		if summary.EvidencePointerID.Valid {
			pointer = summary.EvidencePointerID.String
		}
		input, reason := makeResolvedInput(req, sdkdream.SourceChildDreamSummary, summary.ID, child.OrgScope, pointer, run.ID, summary.SummaryText, visibility, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs, coverage, missing, nil
}

func scopedChildren(children []db.KnowledgeSpace, req ResolveRequest) []db.KnowledgeSpace {
	seen := make(map[string]struct{}, len(children))
	result := make([]db.KnowledgeSpace, 0, len(children))
	for _, child := range children {
		if child.EnterpriseID != req.EnterpriseID || child.ID == "" || child.OrgScope == "" {
			continue
		}
		if _, ok := seen[child.OrgScope]; ok {
			continue
		}
		seen[child.OrgScope] = struct{}{}
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

func scopedSuccessfulRuns(runs []db.DreamRun, req ResolveRequest) []db.DreamRun {
	result := make([]db.DreamRun, 0, len(runs))
	for _, run := range runs {
		if run.EnterpriseID != req.EnterpriseID || run.Status != "succeeded" {
			continue
		}
		if !req.WindowStart.IsZero() && (!run.WindowStart.Valid || !run.WindowStart.Time.Equal(req.WindowStart)) {
			continue
		}
		if !req.WindowEnd.IsZero() && (!run.WindowEnd.Valid || !run.WindowEnd.Time.Equal(req.WindowEnd)) {
			continue
		}
		result = append(result, run)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].OrgUnitID != result[j].OrgUnitID {
			return result[i].OrgUnitID < result[j].OrgUnitID
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func runForChild(runs []db.DreamRun, child db.KnowledgeSpace) (db.DreamRun, bool) {
	for _, run := range runs {
		if sameOrgUnit(child.OrgScope, run.OrgUnitID) {
			return run, true
		}
	}
	return db.DreamRun{}, false
}

func (r childSummaryResolver) summaryForRun(ctx context.Context, enterpriseID, spaceID, runID string) (db.DreamSummary, bool, error) {
	for _, layer := range []string{"retrieval", "display"} {
		summaries, err := r.store.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
			SpaceID: spaceID, Layer: layer, Limit: defaultMaxResolvedInputs,
		})
		if err != nil {
			return db.DreamSummary{}, false, fmt.Errorf("list child %s summaries: %w", layer, err)
		}
		sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
		for _, summary := range summaries {
			if summary.EnterpriseID == enterpriseID && summary.SpaceID == spaceID && summary.RunID == runID && summary.Layer == layer && summary.SummaryText != "" {
				return summary, true, nil
			}
		}
	}
	return db.DreamSummary{}, false, nil
}

func childVisibility(raw []byte, fallbackOrg string) []string {
	var snapshot childVisibilitySnapshot
	if len(raw) == 0 || json.Unmarshal(raw, &snapshot) != nil {
		return []string{fallbackOrg}
	}
	visibility := append([]string(nil), snapshot.OrgUnitIDs...)
	if snapshot.VisibilityLevel != "" {
		visibility = append(visibility, snapshot.VisibilityLevel)
	}
	if len(visibility) == 0 {
		visibility = append(visibility, fallbackOrg)
	}
	return visibility
}
