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

func (r timelineResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]ResolvedInput, Coverage, []MissingInput, error) {
	space, err := r.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     req.OrgUnitID,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("load timeline Dream space: %w", err)
	}
	if space.EnterpriseID != req.EnterpriseID || !sameOrgUnit(space.OrgScope, req.OrgUnitID) {
		return nil, Coverage{}, []MissingInput{{SourceType: r.source, SourceID: req.OrgUnitID, Reason: sdkdream.MissingNotAuthorized}}, nil
	}
	nodes, err := r.store.ListTimelineNodes(ctx, db.ListTimelineNodesParams{
		SpaceID: space.ID,
		Column2: pgtype.Timestamptz{Time: req.WindowStart, Valid: !req.WindowStart.IsZero()},
		Column3: pgtype.Timestamptz{Time: req.WindowEnd, Valid: !req.WindowEnd.IsZero()},
		Limit:   defaultMaxResolvedInputs,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list %s timeline records: %w", r.source, err)
	}
	inputs := make([]ResolvedInput, 0, len(nodes))
	missing := make([]MissingInput, 0)
	for _, node := range nodes {
		if node.EnterpriseID != req.EnterpriseID || node.SpaceID != space.ID || !sameOrgUnit(node.OrgScope, req.OrgUnitID) || node.SourceType != string(r.source) {
			continue
		}
		pointer := ""
		if node.EvidencePointerID.Valid {
			pointer = node.EvidencePointerID.String
		}
		input, reason := makeResolvedInput(req, r.source, node.ID, req.OrgUnitID, pointer, "", node.SummaryText, []string{req.OrgUnitID, node.OrgScope}, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: r.source, SourceID: node.ID, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs, Coverage{}, missing, nil
}
