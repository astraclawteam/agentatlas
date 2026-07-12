package dream

import (
	"context"
	"fmt"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

type timelineResolver struct {
	store  InputStore
	source sdkdream.Source
}

func (r timelineResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]SourceInput, Coverage, []MissingInput, error) {
	space, err := r.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     req.OrgUnitID,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("load timeline Dream space: %w", err)
	}
	if space.EnterpriseID != req.EnterpriseID || !sameOrgUnit(space.OrgScope, req.OrgUnitID) || !scopeKindMatches(space.Kind, space.OrgScope) || (req.SpaceID != "" && req.SpaceID != space.ID) {
		return nil, Coverage{}, []MissingInput{{SourceType: r.source, SourceID: req.OrgUnitID, Reason: sdkdream.MissingNotAuthorized}}, nil
	}
	limit := effectiveInputLimit(req.MaxInputs)
	nodes, err := r.store.ListDreamTimelineNodes(ctx, db.ListDreamTimelineNodesParams{
		EnterpriseID: req.EnterpriseID,
		SpaceID:      space.ID,
		OrgScope:     space.OrgScope,
		SourceType:   string(r.source),
		WindowStart:  pgtype.Timestamptz{Time: req.WindowStart, Valid: true},
		WindowEnd:    pgtype.Timestamptz{Time: req.WindowEnd, Valid: true},
		ResultLimit:  int32(limit + 1),
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list %s timeline records: %w", r.source, err)
	}
	if len(nodes) > limit {
		return nil, Coverage{}, nil, fmt.Errorf("%s timeline records exceed bound %d", r.source, limit)
	}
	inputs := make([]SourceInput, 0, len(nodes))
	missing := make([]MissingInput, 0)
	for _, node := range nodes {
		if node.EnterpriseID != req.EnterpriseID || node.SpaceID != space.ID || !sameOrgUnit(node.OrgScope, req.OrgUnitID) || node.SourceType != string(r.source) || !node.NodeTime.Valid || node.NodeTime.Time.Before(req.WindowStart) || !node.NodeTime.Time.Before(req.WindowEnd) {
			continue
		}
		pointer := ""
		if node.EvidencePointerID.Valid {
			pointer = node.EvidencePointerID.String
		}
		input, reason := makeSourceInput(req, space.ID, r.source, node.ID, req.OrgUnitID, pointer, "", node.SummaryText, []string{req.OrgUnitID, node.OrgScope}, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: r.source, SourceID: node.ID, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs, Coverage{InputCount: len(inputs)}, missing, nil
}
