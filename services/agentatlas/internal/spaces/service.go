package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

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
	ApplyOrgSpaceEvent(ctx context.Context, arg db.ApplyOrgSpaceEventParams) (db.ApplyOrgSpaceEventRow, error)
	UpsertOrgSnapshot(ctx context.Context, arg db.UpsertOrgSnapshotParams) error
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
	if (ev.Scope.ParentID == "") != (ev.Scope.ParentKind == "") {
		return "", false, fmt.Errorf("scope %s parent kind and id must be provided together", ev.Scope.ID)
	}
	scope := ScopeString(ev.Scope.Kind, ev.Scope.ID)
	versionSnapshot, err := json.Marshal(ev.Scope)
	if err != nil {
		return "", false, fmt.Errorf("snapshot scope: %w", err)
	}
	members := normalizeMembers(ev.Members)
	memberSnapshot, err := json.Marshal(members)
	if err != nil {
		return "", false, fmt.Errorf("snapshot members: %w", err)
	}
	result, err := s.store.ApplyOrgSpaceEvent(ctx, db.ApplyOrgSpaceEventParams{
		EventEnterpriseID:    ev.EnterpriseID,
		EventOrgScope:        scope,
		NewSpaceID:           newSpaceID(),
		EventScopeKind:       string(ev.Scope.Kind),
		EventSpaceName:       ev.Scope.Name,
		EventOrgVersion:      ev.OrgVersion,
		EventScopeID:         ev.Scope.ID,
		EventParentScopeKind: pgTextOrEmpty(string(ev.Scope.ParentKind)),
		EventParentScopeID:   pgTextOrEmpty(ev.Scope.ParentID),
		EventVersionSnapshot: versionSnapshot,
		EventMemberSnapshot:  memberSnapshot,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		space, getErr := s.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
			EnterpriseID: ev.EnterpriseID, OrgScope: scope,
		})
		if getErr != nil {
			return "", false, fmt.Errorf("load concurrently created space %s: %w", scope, getErr)
		}
		return space.ID, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("apply org space event %s v%d: %w", scope, ev.OrgVersion, err)
	}
	return result.SpaceID, result.Created, nil
}

func normalizeMembers(members []nexus.OrgMember) []nexus.OrgMember {
	byID := make(map[string]nexus.OrgMember, len(members))
	for _, member := range members {
		if member.UserID != "" {
			byID[member.UserID] = member
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]nexus.OrgMember, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result
}

func (s *Service) ListByEnterprise(ctx context.Context, enterpriseID string) ([]db.KnowledgeSpace, error) {
	return s.store.ListKnowledgeSpacesByEnterprise(ctx, enterpriseID)
}
