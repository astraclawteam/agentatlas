package spaces

import (
	"context"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Store is the persistence surface the space service needs; db/generated
// Queries satisfies it, unit tests use an in-memory fake.
// Store is narrower than it was: GetKnowledgeSpaceByScope, ApplyOrgSpaceEvent
// and UpsertEnterprise existed only to serve space provisioning from org
// events, which the frozen contract makes impossible. The generated queries
// still exist and are still exercised by the integration suite; they are just
// no longer part of what this package depends on.
type Store interface {
	EnsureEnterprise(ctx context.Context, arg db.EnsureEnterpriseParams) error
	UpsertOrgSnapshot(ctx context.Context, arg db.UpsertOrgSnapshotParams) error
	ListKnowledgeSpacesByEnterprise(ctx context.Context, enterpriseID string) ([]db.KnowledgeSpace, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// Space provisioning from the org-event feed was removed here, and it is not
// coming back in this shape. The frozen OrgEvent carries only event_id,
// event_type, org_version and occurred_at -- no scope, no members, no
// enterprise id -- so an event cannot identify the org unit a space would
// belong to. EnsureSpaceFromEvent read all of those off the event and could
// only ever have worked against the earlier proposal contract.
//
// Provisioning a space per org unit needs an authoritative org read surface,
// which the frozen contract does not declare today. Until one exists, spaces
// are created through their own paths and the org feed only advances a
// version cursor (see Syncer).

func (s *Service) ListByEnterprise(ctx context.Context, enterpriseID string) ([]db.KnowledgeSpace, error) {
	return s.store.ListKnowledgeSpacesByEnterprise(ctx, enterpriseID)
}
