package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// These tests replace six that asserted Knowledge-Space provisioning from org
// event payloads. That behaviour was removed, not broken: the frozen OrgEvent
// carries only event_id, event_type, org_version and occurred_at, so an event
// cannot say which org unit changed. The old tests passed because their fake
// events carried a scope and a member list the real server never sends.

type fakeCursorStore struct {
	ensured   []db.EnsureEnterpriseParams
	snapshots []db.UpsertOrgSnapshotParams
	upsertErr error
}

func (f *fakeCursorStore) EnsureEnterprise(_ context.Context, arg db.EnsureEnterpriseParams) error {
	f.ensured = append(f.ensured, arg)
	return nil
}

func (f *fakeCursorStore) UpsertOrgSnapshot(_ context.Context, arg db.UpsertOrgSnapshotParams) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.snapshots = append(f.snapshots, arg)
	return nil
}

func (f *fakeCursorStore) ListKnowledgeSpacesByEnterprise(context.Context, string) ([]db.KnowledgeSpace, error) {
	return nil, nil
}

// replayClient replays queued events. It filters on the cursor ONLY: the real
// feed is scoped by the verified service credential and the event carries no
// enterprise id, so filtering by tenant here would make the double drop events
// the real server delivers.
type replayClient struct {
	events   []nexus.OrgEvent
	gotSince int64
}

func (r *replayClient) VerifyTicket(context.Context, nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	return nexus.VerifyTicketResponse{}, errors.New("not used")
}

func (r *replayClient) AppendAuditEvidence(context.Context, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, errors.New("not used")
}

func (r *replayClient) SubscribeOrgEvents(ctx context.Context, _ string, sinceVersion int64, handler nexus.OrgEventHandler) error {
	r.gotSince = sinceVersion
	for _, ev := range r.events {
		if ev.OrgVersion <= sinceVersion {
			continue
		}
		if err := handler(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func orgEvent(version int64) nexus.OrgEvent {
	return nexus.OrgEvent{
		EventID:    fmt.Sprintf("evt-%d", version),
		EventType:  "org_import",
		OrgVersion: version,
		OccurredAt: time.Unix(1700000000+version, 0).UTC(),
	}
}

func TestSyncerRecordsEveryVersionItReceives(t *testing.T) {
	store := &fakeCursorStore{}
	client := &replayClient{events: []nexus.OrgEvent{orgEvent(1), orgEvent(2), orgEvent(3)}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 0); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(store.snapshots) != 3 {
		t.Fatalf("recorded %d versions, want 3", len(store.snapshots))
	}
	for i, want := range []int64{1, 2, 3} {
		if store.snapshots[i].OrgVersion != want {
			t.Errorf("snapshot %d org_version = %d, want %d", i, store.snapshots[i].OrgVersion, want)
		}
		// The tenant is bound from the subscription, never read off the event:
		// the frozen event has no enterprise id, so deriving it from the event
		// would write rows keyed on the empty string.
		if store.snapshots[i].EnterpriseID != "ent-1" {
			t.Errorf("snapshot %d enterprise = %q, want the subscription tenant", i, store.snapshots[i].EnterpriseID)
		}
	}
}

func TestSyncerResumesStrictlyAfterTheCursor(t *testing.T) {
	store := &fakeCursorStore{}
	client := &replayClient{events: []nexus.OrgEvent{orgEvent(1), orgEvent(2), orgEvent(3)}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if client.gotSince != 2 {
		t.Errorf("subscribed with since_version=%d, want 2", client.gotSince)
	}
	if len(store.snapshots) != 1 || store.snapshots[0].OrgVersion != 3 {
		t.Fatalf("recorded %+v, want only version 3", store.snapshots)
	}
}

// The tenant row is created once per subscription. It used to be created per
// event from the event's own enterprise id, which under the frozen contract is
// always empty -- that inserted an enterprises row with id ''.
func TestSyncerEnsuresTheTenantOncePerSubscriptionNotPerEvent(t *testing.T) {
	store := &fakeCursorStore{}
	client := &replayClient{events: []nexus.OrgEvent{orgEvent(1), orgEvent(2), orgEvent(3)}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 0); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(store.ensured) != 1 {
		t.Fatalf("EnsureEnterprise called %d times, want exactly 1", len(store.ensured))
	}
	if store.ensured[0].ID != "ent-1" {
		t.Errorf("ensured enterprise %q, want the subscription tenant", store.ensured[0].ID)
	}
}

// An unknown event type must advance the cursor, not stop the stream. The
// contract requires consumers to "ignore unknown values rather than fail closed
// on them", and the only type the server emits today is "org_import" -- a
// consumer that switched on a closed set would reject every future one.
func TestSyncerRecordsUnknownEventTypesInsteadOfFailing(t *testing.T) {
	store := &fakeCursorStore{}
	odd := orgEvent(1)
	odd.EventType = "something_this_build_has_never_heard_of"
	client := &replayClient{events: []nexus.OrgEvent{odd}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 0); err != nil {
		t.Fatalf("an unknown event type must not stop the stream: %v", err)
	}
	if len(store.snapshots) != 1 {
		t.Fatalf("recorded %d versions, want the unknown type still to advance the cursor", len(store.snapshots))
	}
}

func TestSyncerPersistsTheFrozenEventShapeOnly(t *testing.T) {
	store := &fakeCursorStore{}
	client := &replayClient{events: []nexus.OrgEvent{orgEvent(7)}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 0); err != nil {
		t.Fatalf("run: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(store.snapshots[0].Snapshot, &decoded); err != nil {
		t.Fatalf("snapshot is not valid json: %v", err)
	}
	for _, forbidden := range []string{"scope", "members", "enterprise_id", "payload", "source_hash"} {
		if _, present := decoded[forbidden]; present {
			t.Errorf("persisted snapshot carries %q, which the frozen OrgEvent does not publish", forbidden)
		}
	}
	for _, required := range []string{"event_id", "event_type", "org_version", "occurred_at"} {
		if _, present := decoded[required]; !present {
			t.Errorf("persisted snapshot is missing %q", required)
		}
	}
}

func TestSyncerStopsWhenTheCursorCannotBePersisted(t *testing.T) {
	store := &fakeCursorStore{upsertErr: errors.New("db down")}
	client := &replayClient{events: []nexus.OrgEvent{orgEvent(1), orgEvent(2)}}
	syncer := NewSyncer(store, client, zap.NewNop())

	if err := syncer.Run(context.Background(), "ent-1", 0); err == nil {
		t.Fatal("a failed cursor write must stop the stream, not be skipped")
	}
	if len(store.snapshots) != 0 {
		t.Errorf("recorded %d versions despite the write failing", len(store.snapshots))
	}
}
