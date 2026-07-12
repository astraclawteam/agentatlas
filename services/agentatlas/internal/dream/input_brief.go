package dream

import (
	"context"
	"fmt"
	"sort"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

type workBriefResolver struct{ store InputStore }

func (r workBriefResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]ResolvedInput, Coverage, []MissingInput, error) {
	space, err := r.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     req.OrgUnitID,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("load direct Dream space: %w", err)
	}
	if space.EnterpriseID != req.EnterpriseID || !sameOrgUnit(space.OrgScope, req.OrgUnitID) || !directBriefScope(space.Kind) {
		return nil, Coverage{}, []MissingInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: req.OrgUnitID, Reason: sdkdream.MissingNotAuthorized}}, nil
	}
	members, err := r.store.ListSpaceMembers(ctx, space.ID)
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list direct Dream members: %w", err)
	}
	memberIDs := make([]string, 0, len(members))
	memberSet := make(map[string]struct{}, len(members))
	for _, member := range members {
		if member.SpaceID != space.ID || member.UserID == "" {
			continue
		}
		if _, ok := memberSet[member.UserID]; ok {
			continue
		}
		memberSet[member.UserID] = struct{}{}
		memberIDs = append(memberIDs, member.UserID)
	}
	if len(memberIDs) == 0 {
		return nil, Coverage{}, nil, nil
	}
	sort.Strings(memberIDs)
	briefs, err := r.store.ListWorkBriefsForWindow(ctx, db.ListWorkBriefsForWindowParams{
		EnterpriseID: req.EnterpriseID,
		Column2:      memberIDs,
		BriefDate:    pgtype.Date{Time: req.WindowStart, Valid: !req.WindowStart.IsZero()},
		BriefDate_2:  pgtype.Date{Time: req.WindowEnd, Valid: !req.WindowEnd.IsZero()},
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list direct Dream briefs: %w", err)
	}
	inputs := make([]ResolvedInput, 0, len(briefs))
	missing := make([]MissingInput, 0)
	for _, brief := range briefs {
		if brief.EnterpriseID != req.EnterpriseID {
			continue
		}
		if _, ok := memberSet[brief.EmployeeUserID]; !ok {
			continue
		}
		input, reason := makeResolvedInput(req, sdkdream.SourceWorkBrief, brief.ID, req.OrgUnitID, brief.EvidencePointerID, "", brief.Summary,
			[]string{req.OrgUnitID, space.OrgScope, brief.EmployeeUserID}, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceWorkBrief, SourceID: brief.ID, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs, Coverage{}, missing, nil
}
