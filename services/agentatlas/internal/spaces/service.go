package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Store is the persistence surface the space service needs; db/generated
// Queries satisfies it, unit tests use an in-memory fake.
type Store interface {
	EnsureEnterprise(ctx context.Context, arg db.EnsureEnterpriseParams) error
	UpsertEnterprise(ctx context.Context, arg db.UpsertEnterpriseParams) (db.Enterprise, error)
	GetKnowledgeSpaceByScope(ctx context.Context, arg db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error)
	InsertKnowledgeSpace(ctx context.Context, arg db.InsertKnowledgeSpaceParams) (db.KnowledgeSpace, error)
	UpdateKnowledgeSpaceIfNewer(ctx context.Context, arg db.UpdateKnowledgeSpaceIfNewerParams) (int64, error)
	InsertKnowledgeSpaceVersion(ctx context.Context, arg db.InsertKnowledgeSpaceVersionParams) error
	UpsertOrgScopeBinding(ctx context.Context, arg db.UpsertOrgScopeBindingParams) error
	UpsertOrgSnapshot(ctx context.Context, arg db.UpsertOrgSnapshotParams) error
	UpsertSpaceMember(ctx context.Context, arg db.UpsertSpaceMemberParams) error
	ListKnowledgeSpacesByEnterprise(ctx context.Context, enterpriseID string) ([]db.KnowledgeSpace, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// EnsureSpaceFromEvent creates or refreshes the knowledge space bound to the
// event's org scope. Stale events (org_version older than the stored space)
// never overwrite newer state; replays are idempotent.
func (s *Service) EnsureSpaceFromEvent(ctx context.Context, ev nexus.OrgEvent) (spaceID string, created bool, err error) {
	scope := ScopeString(ev.Scope.Kind, ev.Scope.ID)

	existing, err := s.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: ev.EnterpriseID,
		OrgScope:     scope,
	})
	switch {
	case err == nil:
		rows, uerr := s.store.UpdateKnowledgeSpaceIfNewer(ctx, db.UpdateKnowledgeSpaceIfNewerParams{
			ID:           existing.ID,
			EnterpriseID: ev.EnterpriseID,
			Name:         ev.Scope.Name,
			OrgVersion:   ev.OrgVersion,
		})
		if uerr != nil {
			return "", false, fmt.Errorf("update space %s: %w", existing.ID, uerr)
		}
		if rows > 0 {
			if verr := s.recordVersion(ctx, existing.ID, ev); verr != nil {
				return "", false, verr
			}
		}
		spaceID = existing.ID
	case errors.Is(err, pgx.ErrNoRows):
		space, ierr := s.store.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
			ID:           newSpaceID(),
			EnterpriseID: ev.EnterpriseID,
			Kind:         string(ev.Scope.Kind),
			Name:         ev.Scope.Name,
			OrgScope:     scope,
			OrgVersion:   ev.OrgVersion,
		})
		if ierr != nil {
			return "", false, fmt.Errorf("insert space for %s: %w", scope, ierr)
		}
		if berr := s.store.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{
			EnterpriseID:  ev.EnterpriseID,
			SpaceID:       space.ID,
			ScopeKind:     string(ev.Scope.Kind),
			ScopeID:       ev.Scope.ID,
			ParentScopeID: pgTextOrEmpty(ev.Scope.ParentID),
		}); berr != nil {
			return "", false, fmt.Errorf("bind scope %s: %w", scope, berr)
		}
		if verr := s.recordVersion(ctx, space.ID, ev); verr != nil {
			return "", false, verr
		}
		spaceID, created = space.ID, true
	default:
		return "", false, fmt.Errorf("get space by scope %s: %w", scope, err)
	}

	for _, member := range ev.Members {
		if merr := s.store.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{
			SpaceID:     spaceID,
			UserID:      member.UserID,
			DisplayName: member.DisplayName,
			OrgVersion:  ev.OrgVersion,
		}); merr != nil {
			return "", false, fmt.Errorf("member %s: %w", member.UserID, merr)
		}
	}
	return spaceID, created, nil
}

func (s *Service) recordVersion(ctx context.Context, spaceID string, ev nexus.OrgEvent) error {
	snapshot, err := json.Marshal(ev.Scope)
	if err != nil {
		return fmt.Errorf("snapshot scope: %w", err)
	}
	if err := s.store.InsertKnowledgeSpaceVersion(ctx, db.InsertKnowledgeSpaceVersionParams{
		SpaceID:    spaceID,
		OrgVersion: ev.OrgVersion,
		Snapshot:   snapshot,
	}); err != nil {
		return fmt.Errorf("record space version: %w", err)
	}
	return nil
}

func (s *Service) ListByEnterprise(ctx context.Context, enterpriseID string) ([]db.KnowledgeSpace, error) {
	return s.store.ListKnowledgeSpacesByEnterprise(ctx, enterpriseID)
}
