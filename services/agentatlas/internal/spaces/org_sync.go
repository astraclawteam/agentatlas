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

// Syncer records the tenant's sealed organization version from the AgentNexus
// org-event feed.
//
// It used to provision Knowledge Spaces from event payloads. It cannot: the
// frozen OrgEvent carries only event_id, event_type, org_version and
// occurred_at, with additionalProperties:false and payloads explicitly
// forbidden, so an event does not say WHICH org unit changed. Space
// provisioning from this feed is not a wiring gap, it is impossible by
// contract, and it would need an authoritative org read surface that the
// frozen contract does not currently declare.
//
// What the feed does support is exactly what it was designed for: a durable
// per-tenant version cursor. Recording it lets a consumer resume strictly
// after the last version it applied, and lets anything that caches org-derived
// state know that its cache is stale.
type Syncer struct {
	nexus nexus.Client
	store Store
	log   *zap.Logger
}

func NewSyncer(store Store, client nexus.Client, log *zap.Logger) *Syncer {
	return &Syncer{nexus: client, store: store, log: log}
}

// Run subscribes from sinceVersion and records versions until the stream ends
// or the context is cancelled. Callers persist the last recorded version and
// resume with it.
//
// enterpriseID names the tenant this subscription belongs to for local
// bookkeeping only. It is NOT sent to AgentNexus: the contract derives tenant
// scope from the verified service credential and declares since_version as the
// only parameter. It is bound here rather than read off each event because the
// frozen event does not carry it.
func (s *Syncer) Run(ctx context.Context, enterpriseID string, sinceVersion int64) error {
	// FK safety, once per subscription rather than once per event. Doing it
	// per event used to read the enterprise id off the event itself, which
	// under the frozen contract is always empty -- that would insert an
	// enterprises row with id ''.
	if err := s.store.EnsureEnterprise(ctx, db.EnsureEnterpriseParams{
		ID:   enterpriseID,
		Name: enterpriseID,
	}); err != nil {
		return fmt.Errorf("ensure enterprise %s: %w", enterpriseID, err)
	}
	return s.nexus.SubscribeOrgEvents(ctx, enterpriseID, sinceVersion, func(ctx context.Context, ev nexus.OrgEvent) error {
		return s.record(ctx, enterpriseID, ev)
	})
}

// record persists one event as the tenant's org-version watermark. It is
// idempotent: replaying a version rewrites the same row.
//
// It deliberately does not branch on EventType. The contract requires that
// consumers "ignore unknown values rather than fail closed on them", and the
// cursor advances identically whatever the change was.
func (s *Syncer) record(ctx context.Context, enterpriseID string, ev nexus.OrgEvent) error {
	snapshot, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal org event: %w", err)
	}
	if err := s.store.UpsertOrgSnapshot(ctx, db.UpsertOrgSnapshotParams{
		EnterpriseID: enterpriseID,
		OrgVersion:   ev.OrgVersion,
		Snapshot:     snapshot,
	}); err != nil {
		return fmt.Errorf("org version v%d: %w", ev.OrgVersion, err)
	}
	s.log.Info("org version recorded",
		zap.String("enterprise_id", enterpriseID),
		zap.String("event_id", ev.EventID),
		zap.String("event_type", ev.EventType),
		zap.Int64("org_version", ev.OrgVersion),
	)
	return nil
}
