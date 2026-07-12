package dream

import (
	"context"
	"fmt"
	"sort"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

type workBriefResolver struct{ store InputStore }

func (r workBriefResolver) ResolveSource(ctx context.Context, req ResolveRequest, masker *Masker) ([]SourceInput, Coverage, []MissingInput, error) {
	space, err := r.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     req.OrgUnitID,
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("load direct Dream space: %w", err)
	}
	if space.EnterpriseID != req.EnterpriseID || !sameOrgUnit(space.OrgScope, req.OrgUnitID) || !directBriefScope(space.Kind) || !scopeKindMatches(space.Kind, space.OrgScope) || (req.SpaceID != "" && req.SpaceID != space.ID) {
		return nil, Coverage{}, []MissingInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: req.OrgUnitID, Reason: sdkdream.MissingNotAuthorized}}, nil
	}
	limit := effectiveInputLimit(req.MaxInputs)
	members, err := r.store.ListDreamSpaceMembers(ctx, db.ListDreamSpaceMembersParams{SpaceID: space.ID, ResultLimit: int32(limit + 1)})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list direct Dream members: %w", err)
	}
	if len(members) > limit {
		return nil, Coverage{}, nil, fmt.Errorf("direct Dream members exceed bound %d", limit)
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
	briefs, err := r.store.ListDreamWorkBriefsForWindow(ctx, db.ListDreamWorkBriefsForWindowParams{
		EnterpriseID:    req.EnterpriseID,
		EmployeeUserIds: memberIDs,
		WindowStart:     pgtype.Date{Time: req.WindowStart, Valid: true},
		WindowEnd:       pgtype.Date{Time: req.WindowEnd, Valid: true},
		ResultLimit:     int32(limit + 1),
	})
	if err != nil {
		return nil, Coverage{}, nil, fmt.Errorf("list direct Dream briefs: %w", err)
	}
	if len(briefs) > limit {
		return nil, Coverage{}, nil, fmt.Errorf("direct Dream briefs exceed bound %d", limit)
	}
	inputs := make([]SourceInput, 0, len(briefs))
	missing := make([]MissingInput, 0)
	for _, brief := range briefs {
		if brief.EnterpriseID != req.EnterpriseID {
			continue
		}
		if !brief.BriefDate.Valid || !dateInWindow(brief.BriefDate.Time, req.WindowStart, req.WindowEnd) {
			continue
		}
		if _, ok := memberSet[brief.EmployeeUserID]; !ok {
			continue
		}
		input, reason := makeSourceInput(req, space.ID, sdkdream.SourceWorkBrief, brief.ID, req.OrgUnitID, brief.EvidencePointerID, "", brief.Summary,
			[]string{req.OrgUnitID, space.OrgScope, brief.EmployeeUserID}, masker)
		if reason != "" {
			missing = append(missing, MissingInput{SourceType: sdkdream.SourceWorkBrief, SourceID: brief.ID, Reason: reason})
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs, Coverage{InputCount: len(inputs)}, missing, nil
}

func dateInWindow(value, start, end time.Time) bool {
	date := time.Date(value.UTC().Year(), value.UTC().Month(), value.UTC().Day(), 0, 0, 0, 0, time.UTC)
	startDate := time.Date(start.UTC().Year(), start.UTC().Month(), start.UTC().Day(), 0, 0, 0, 0, time.UTC)
	endDate := time.Date(end.UTC().Year(), end.UTC().Month(), end.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return !date.Before(startDate) && date.Before(endDate)
}
