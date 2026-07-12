package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ApplyOrgSpaceEventParams struct {
	EventEnterpriseID    string      `json:"event_enterprise_id"`
	EventOrgScope        string      `json:"event_org_scope"`
	NewSpaceID           string      `json:"new_space_id"`
	EventScopeKind       string      `json:"event_scope_kind"`
	EventSpaceName       string      `json:"event_space_name"`
	EventOrgVersion      int64       `json:"event_org_version"`
	EventScopeID         string      `json:"event_scope_id"`
	EventParentScopeKind pgtype.Text `json:"event_parent_scope_kind"`
	EventParentScopeID   pgtype.Text `json:"event_parent_scope_id"`
	EventVersionSnapshot []byte      `json:"event_version_snapshot"`
	EventMemberSnapshot  []byte      `json:"event_member_snapshot"`
}

type ApplyOrgSpaceEventRow struct {
	SpaceID  string `json:"space_id"`
	Accepted bool   `json:"accepted"`
	Created  bool   `json:"created"`
}

type orgSpaceMember struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}

// ApplyOrgSpaceEvent serializes mutations for one enterprise org scope before
// reading any related table. Separate statements then receive fresh READ
// COMMITTED snapshots while the transaction preserves all-or-nothing updates.
func (q *Queries) ApplyOrgSpaceEvent(ctx context.Context, arg ApplyOrgSpaceEventParams) (result ApplyOrgSpaceEventRow, err error) {
	err = q.InTransactionWithOptions(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(txq *Queries) error {
		if err := txq.LockOrgSpaceMutation(ctx, LockOrgSpaceMutationParams{
			LockEnterpriseID: arg.EventEnterpriseID,
			LockOrgScope:     arg.EventOrgScope,
		}); err != nil {
			return fmt.Errorf("lock org space mutation: %w", err)
		}

		existing, getErr := txq.GetKnowledgeSpaceByScope(ctx, GetKnowledgeSpaceByScopeParams{
			EnterpriseID: arg.EventEnterpriseID,
			OrgScope:     arg.EventOrgScope,
		})
		created := false
		switch {
		case getErr == nil && existing.OrgVersion >= arg.EventOrgVersion:
			result = ApplyOrgSpaceEventRow{SpaceID: existing.ID}
			return nil
		case getErr == nil:
			updated, updateErr := txq.UpdateKnowledgeSpaceIfNewer(ctx, UpdateKnowledgeSpaceIfNewerParams{
				ID:           existing.ID,
				EnterpriseID: arg.EventEnterpriseID,
				Name:         arg.EventSpaceName,
				OrgVersion:   arg.EventOrgVersion,
			})
			if updateErr != nil {
				return updateErr
			}
			if updated != 1 {
				return fmt.Errorf("update org space %s affected %d rows", arg.EventOrgScope, updated)
			}
		case errors.Is(getErr, pgx.ErrNoRows):
			inserted, insertErr := txq.InsertKnowledgeSpace(ctx, InsertKnowledgeSpaceParams{
				ID:           arg.NewSpaceID,
				EnterpriseID: arg.EventEnterpriseID,
				Kind:         arg.EventScopeKind,
				Name:         arg.EventSpaceName,
				OrgScope:     arg.EventOrgScope,
				OrgVersion:   arg.EventOrgVersion,
			})
			if insertErr != nil {
				return insertErr
			}
			existing = inserted
			created = true
		default:
			return getErr
		}

		if err := txq.UpsertOrgScopeBinding(ctx, UpsertOrgScopeBindingParams{
			EnterpriseID:    arg.EventEnterpriseID,
			SpaceID:         existing.ID,
			ScopeKind:       arg.EventScopeKind,
			ScopeID:         arg.EventScopeID,
			ParentScopeKind: arg.EventParentScopeKind,
			ParentScopeID:   arg.EventParentScopeID,
		}); err != nil {
			return err
		}
		if err := txq.InsertKnowledgeSpaceVersion(ctx, InsertKnowledgeSpaceVersionParams{
			SpaceID: existing.ID, OrgVersion: arg.EventOrgVersion, Snapshot: arg.EventVersionSnapshot,
		}); err != nil {
			return err
		}
		var members []orgSpaceMember
		if err := json.Unmarshal(arg.EventMemberSnapshot, &members); err != nil {
			return fmt.Errorf("decode org space members: %w", err)
		}
		for _, member := range members {
			if err := txq.UpsertSpaceMember(ctx, UpsertSpaceMemberParams{
				SpaceID: existing.ID, UserID: member.UserID, DisplayName: member.DisplayName, OrgVersion: arg.EventOrgVersion,
			}); err != nil {
				return err
			}
		}
		if _, err := txq.DeleteSpaceMembersStale(ctx, DeleteSpaceMembersStaleParams{
			SpaceID: existing.ID, OrgVersion: arg.EventOrgVersion,
		}); err != nil {
			return err
		}
		result = ApplyOrgSpaceEventRow{SpaceID: existing.ID, Accepted: true, Created: created}
		return nil
	})
	return result, err
}
