package spaces

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

func pgTextOrEmpty(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// Syncer consumes AgentNexus org events and keeps knowledge spaces aligned
// with the org graph.
type Syncer struct {
	service *Service
	nexus   nexus.Client
	store   Store
	log     *zap.Logger
}

func NewSyncer(service *Service, store Store, client nexus.Client, log *zap.Logger) *Syncer {
	return &Syncer{service: service, nexus: client, store: store, log: log}
}

// Run subscribes from sinceVersion and processes events until the stream ends
// or the context is cancelled. Callers persist the last processed version and
// resume with it.
func (s *Syncer) Run(ctx context.Context, enterpriseID string, sinceVersion int64) error {
	return s.nexus.SubscribeOrgEvents(ctx, enterpriseID, sinceVersion, s.Handle)
}

// Handle processes one org event idempotently.
func (s *Syncer) Handle(ctx context.Context, ev nexus.OrgEvent) error {
	// FK safety: every event may arrive before an explicit company event.
	if err := s.store.EnsureEnterprise(ctx, db.EnsureEnterpriseParams{
		ID:   ev.EnterpriseID,
		Name: ev.EnterpriseID,
	}); err != nil {
		return fmt.Errorf("ensure enterprise %s: %w", ev.EnterpriseID, err)
	}

	switch ev.Type {
	case nexus.OrgCompanyUpserted:
		if _, err := s.store.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{
			ID:   ev.EnterpriseID,
			Name: ev.Scope.Name,
		}); err != nil {
			return fmt.Errorf("upsert enterprise %s: %w", ev.EnterpriseID, err)
		}
		fallthrough
	case nexus.OrgEmployeeUpserted, nexus.OrgProjectGroupUpserted,
		nexus.OrgDepartmentUpserted, nexus.OrgBusinessUnitUpserted:
		spaceID, created, err := s.service.EnsureSpaceFromEvent(ctx, ev)
		if err != nil {
			return err
		}
		s.log.Info("org space synced",
			zap.String("enterprise_id", ev.EnterpriseID),
			zap.String("space_id", spaceID),
			zap.String("org_scope", ScopeString(ev.Scope.Kind, ev.Scope.ID)),
			zap.Int64("org_version", ev.OrgVersion),
			zap.Bool("created", created),
		)
	case nexus.OrgEmployeeRemoved, nexus.OrgProjectGroupRemoved,
		nexus.OrgDepartmentRemoved, nexus.OrgBusinessUnitRemoved:
		// Spaces are retained for organizational memory; visibility of their
		// content is always enforced by AgentNexus, so a removed scope simply
		// stops receiving new inputs.
		s.log.Info("org scope removed; space retained",
			zap.String("enterprise_id", ev.EnterpriseID),
			zap.String("org_scope", ScopeString(ev.Scope.Kind, ev.Scope.ID)),
			zap.Int64("org_version", ev.OrgVersion),
		)
	default:
		return fmt.Errorf("unknown org event type %q (version %d)", ev.Type, ev.OrgVersion)
	}

	snapshot, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal org snapshot: %w", err)
	}
	if err := s.store.UpsertOrgSnapshot(ctx, db.UpsertOrgSnapshotParams{
		EnterpriseID: ev.EnterpriseID,
		OrgVersion:   ev.OrgVersion,
		Snapshot:     snapshot,
	}); err != nil {
		return fmt.Errorf("org snapshot v%d: %w", ev.OrgVersion, err)
	}
	return nil
}
