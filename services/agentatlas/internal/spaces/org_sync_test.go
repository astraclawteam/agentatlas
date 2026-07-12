package spaces

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// fakeStore is an in-memory Store honoring the same uniqueness and
// version-guard semantics as the real schema.
type fakeStore struct {
	mu                 sync.Mutex
	applyMu            sync.Mutex
	enterprises        map[string]db.Enterprise
	spaces             map[string]db.KnowledgeSpace // key enterprise|org_scope
	byID               map[string]db.KnowledgeSpace
	versions           []db.InsertKnowledgeSpaceVersionParams
	bindings           map[string]db.UpsertOrgScopeBindingParams // key enterprise|kind|scope_id
	snapshots          map[string]int64                          // enterprise -> max version seen
	members            map[string]db.UpsertSpaceMemberParams     // key space|user
	failBindingOnce    bool
	bindingBlockParent string
	bindingEntered     chan struct{}
	bindingRelease     chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		enterprises: map[string]db.Enterprise{},
		spaces:      map[string]db.KnowledgeSpace{},
		byID:        map[string]db.KnowledgeSpace{},
		bindings:    map[string]db.UpsertOrgScopeBindingParams{},
		snapshots:   map[string]int64{},
		members:     map[string]db.UpsertSpaceMemberParams{},
	}
}

func scopeKey(ent, scope string) string { return ent + "|" + scope }

func (f *fakeStore) EnsureEnterprise(_ context.Context, arg db.EnsureEnterpriseParams) error {
	if _, ok := f.enterprises[arg.ID]; !ok {
		f.enterprises[arg.ID] = db.Enterprise{ID: arg.ID, Name: arg.Name}
	}
	return nil
}

func (f *fakeStore) UpsertEnterprise(_ context.Context, arg db.UpsertEnterpriseParams) (db.Enterprise, error) {
	e := db.Enterprise{ID: arg.ID, Name: arg.Name}
	f.enterprises[arg.ID] = e
	return e, nil
}

func (f *fakeStore) GetKnowledgeSpaceByScope(_ context.Context, arg db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.spaces[scopeKey(arg.EnterpriseID, arg.OrgScope)]; ok {
		return s, nil
	}
	return db.KnowledgeSpace{}, pgx.ErrNoRows
}

func (f *fakeStore) ApplyOrgSpaceEvent(_ context.Context, arg db.ApplyOrgSpaceEventParams) (db.ApplyOrgSpaceEventRow, error) {
	f.applyMu.Lock()
	defer f.applyMu.Unlock()

	f.mu.Lock()
	existing, exists := f.spaces[scopeKey(arg.EventEnterpriseID, arg.EventOrgScope)]
	if exists && existing.OrgVersion >= arg.EventOrgVersion {
		f.mu.Unlock()
		return db.ApplyOrgSpaceEventRow{SpaceID: existing.ID}, nil
	}
	if f.failBindingOnce {
		f.failBindingOnce = false
		f.mu.Unlock()
		return db.ApplyOrgSpaceEventRow{}, fmt.Errorf("injected binding failure")
	}
	block := f.bindingBlockParent != "" && arg.EventParentScopeID.Valid && arg.EventParentScopeID.String == f.bindingBlockParent
	entered, release := f.bindingEntered, f.bindingRelease
	f.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}

	var members []nexus.OrgMember
	if err := json.Unmarshal(arg.EventMemberSnapshot, &members); err != nil {
		return db.ApplyOrgSpaceEventRow{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	space := existing
	created := !exists
	if created {
		space = db.KnowledgeSpace{
			ID: arg.NewSpaceID, EnterpriseID: arg.EventEnterpriseID, Kind: arg.EventScopeKind,
			Name: arg.EventSpaceName, OrgScope: arg.EventOrgScope, OrgVersion: arg.EventOrgVersion,
		}
	} else {
		space.Name, space.OrgVersion = arg.EventSpaceName, arg.EventOrgVersion
	}
	f.spaces[scopeKey(space.EnterpriseID, space.OrgScope)] = space
	f.byID[space.ID] = space
	f.bindings[arg.EventEnterpriseID+"|"+arg.EventScopeKind+"|"+arg.EventScopeID] = db.UpsertOrgScopeBindingParams{
		EnterpriseID: arg.EventEnterpriseID, SpaceID: space.ID, ScopeKind: arg.EventScopeKind, ScopeID: arg.EventScopeID,
		ParentScopeKind: arg.EventParentScopeKind, ParentScopeID: arg.EventParentScopeID,
	}
	f.versions = append(f.versions, db.InsertKnowledgeSpaceVersionParams{
		SpaceID: space.ID, OrgVersion: arg.EventOrgVersion, Snapshot: arg.EventVersionSnapshot,
	})
	memberPrefix := space.ID + "|"
	for key := range f.members {
		if strings.HasPrefix(key, memberPrefix) {
			delete(f.members, key)
		}
	}
	for _, member := range members {
		f.members[memberPrefix+member.UserID] = db.UpsertSpaceMemberParams{
			SpaceID: space.ID, UserID: member.UserID, DisplayName: member.DisplayName, OrgVersion: arg.EventOrgVersion,
		}
	}
	return db.ApplyOrgSpaceEventRow{SpaceID: space.ID, Accepted: true, Created: created}, nil
}

func (f *fakeStore) InsertKnowledgeSpace(_ context.Context, arg db.InsertKnowledgeSpaceParams) (db.KnowledgeSpace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := db.KnowledgeSpace{
		ID: arg.ID, EnterpriseID: arg.EnterpriseID, Kind: arg.Kind,
		Name: arg.Name, OrgScope: arg.OrgScope, OrgVersion: arg.OrgVersion,
	}
	f.spaces[scopeKey(arg.EnterpriseID, arg.OrgScope)] = s
	f.byID[arg.ID] = s
	return s, nil
}

func (f *fakeStore) UpdateKnowledgeSpaceIfNewer(_ context.Context, arg db.UpdateKnowledgeSpaceIfNewerParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[arg.ID]
	if !ok || s.OrgVersion >= arg.OrgVersion {
		return 0, nil
	}
	s.Name = arg.Name
	s.OrgVersion = arg.OrgVersion
	f.byID[arg.ID] = s
	f.spaces[scopeKey(s.EnterpriseID, s.OrgScope)] = s
	return 1, nil
}

func (f *fakeStore) InsertKnowledgeSpaceVersion(_ context.Context, arg db.InsertKnowledgeSpaceVersionParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions = append(f.versions, arg)
	return nil
}

func (f *fakeStore) UpsertOrgScopeBinding(_ context.Context, arg db.UpsertOrgScopeBindingParams) error {
	f.mu.Lock()
	if f.failBindingOnce {
		f.failBindingOnce = false
		f.mu.Unlock()
		return fmt.Errorf("injected binding failure")
	}
	block := f.bindingBlockParent != "" && arg.ParentScopeID.Valid && arg.ParentScopeID.String == f.bindingBlockParent
	entered, release := f.bindingEntered, f.bindingRelease
	f.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[arg.EnterpriseID+"|"+arg.ScopeKind+"|"+arg.ScopeID] = arg
	return nil
}

func (f *fakeStore) UpsertOrgSnapshot(_ context.Context, arg db.UpsertOrgSnapshotParams) error {
	if arg.OrgVersion > f.snapshots[arg.EnterpriseID] {
		f.snapshots[arg.EnterpriseID] = arg.OrgVersion
	}
	return nil
}

func (f *fakeStore) UpsertSpaceMember(_ context.Context, arg db.UpsertSpaceMemberParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := arg.SpaceID + "|" + arg.UserID
	if existing, ok := f.members[key]; ok && existing.OrgVersion > arg.OrgVersion {
		return nil
	}
	f.members[key] = arg
	return nil
}

func (f *fakeStore) ListKnowledgeSpacesByEnterprise(_ context.Context, enterpriseID string) ([]db.KnowledgeSpace, error) {
	var out []db.KnowledgeSpace
	for _, s := range f.spaces {
		if s.EnterpriseID == enterpriseID {
			out = append(out, s)
		}
	}
	return out, nil
}

func orgEvent(t nexus.OrgEventType, version int64, kind nexus.OrgScopeKind, id, name string, members ...nexus.OrgMember) nexus.OrgEvent {
	return nexus.OrgEvent{
		EventID: "evt_" + id, EnterpriseID: "ent_1", OrgVersion: version,
		Type: t, Scope: nexus.OrgScope{Kind: kind, ID: id, Name: name},
		Members: members, OccurredAt: time.Unix(1750000000+version, 0),
	}
}

func newTestSyncer(events ...nexus.OrgEvent) (*Syncer, *fakeStore) {
	store := newFakeStore()
	// Mock client inline to avoid an import cycle with internal/nexusclient.
	client := &replayClient{events: events}
	svc := NewService(store)
	return NewSyncer(svc, store, client, zap.NewNop()), store
}

type replayClient struct{ events []nexus.OrgEvent }

func (r *replayClient) VerifyTicket(context.Context, nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	return nexus.VerifyTicketResponse{}, nil
}
func (r *replayClient) LocateEvidence(context.Context, nexus.LocateEvidenceRequest) (nexus.LocateEvidenceResponse, error) {
	return nexus.LocateEvidenceResponse{}, nil
}
func (r *replayClient) ReadEvidence(context.Context, nexus.ReadEvidenceRequest) (nexus.ReadEvidenceResponse, error) {
	return nexus.ReadEvidenceResponse{}, nil
}
func (r *replayClient) AppendAuditEvidence(context.Context, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "a"}, nil
}
func (r *replayClient) SubscribeOrgEvents(ctx context.Context, ent string, since int64, h nexus.OrgEventHandler) error {
	for _, ev := range r.events {
		if ev.EnterpriseID == ent && ev.OrgVersion > since {
			if err := h(ctx, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestOrgSyncCreatesSpaces(t *testing.T) {
	events := []nexus.OrgEvent{
		orgEvent(nexus.OrgCompanyUpserted, 1, nexus.ScopeCompany, "c1", "顺视智能制造集团"),
		orgEvent(nexus.OrgEmployeeUpserted, 2, nexus.ScopeEmployee, "u_zhang", "张予安",
			nexus.OrgMember{UserID: "u_zhang", DisplayName: "张予安"}),
		orgEvent(nexus.OrgProjectGroupUpserted, 3, nexus.ScopeProjectGroup, "pg_mes", "MES 异常工单专项组",
			nexus.OrgMember{UserID: "u_zhang"}, nexus.OrgMember{UserID: "u_li"}),
		orgEvent(nexus.OrgDepartmentUpserted, 4, nexus.ScopeDepartment, "dep_rd1", "研发一部"),
	}
	syncer, store := newTestSyncer(events...)

	if err := syncer.Run(context.Background(), "ent_1", 0); err != nil {
		t.Fatalf("run: %v", err)
	}

	all, _ := store.ListKnowledgeSpacesByEnterprise(context.Background(), "ent_1")
	if len(all) != 4 {
		t.Fatalf("spaces = %d, want 4 (company/employee/project_group/department)", len(all))
	}
	kinds := map[string]bool{}
	for _, s := range all {
		kinds[s.Kind] = true
	}
	for _, k := range []string{"company", "employee", "project_group", "department"} {
		if !kinds[k] {
			t.Fatalf("missing space kind %q", k)
		}
	}
	if store.enterprises["ent_1"].Name != "顺视智能制造集团" {
		t.Fatalf("enterprise name = %q", store.enterprises["ent_1"].Name)
	}
	pg := store.spaces[scopeKey("ent_1", "project_group:pg_mes")]
	if _, ok := store.members[pg.ID+"|u_li"]; !ok {
		t.Fatal("project group member u_li not cached")
	}
	if store.snapshots["ent_1"] != 4 {
		t.Fatalf("org snapshot version = %d, want 4", store.snapshots["ent_1"])
	}
}

func TestOrgSyncStaleEventDoesNotOverwrite(t *testing.T) {
	fresh := orgEvent(nexus.OrgEmployeeUpserted, 10, nexus.ScopeEmployee, "u_zhang", "张予安")
	stale := orgEvent(nexus.OrgEmployeeUpserted, 3, nexus.ScopeEmployee, "u_zhang", "旧名字")
	syncer, store := newTestSyncer()

	if err := syncer.Handle(context.Background(), fresh); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := syncer.Handle(context.Background(), stale); err != nil {
		t.Fatalf("stale: %v", err)
	}

	s := store.spaces[scopeKey("ent_1", "employee:u_zhang")]
	if s.Name != "张予安" || s.OrgVersion != 10 {
		t.Fatalf("stale event overwrote newer state: %+v", s)
	}
}

func TestOrgSyncIdempotentReplay(t *testing.T) {
	ev := orgEvent(nexus.OrgProjectGroupUpserted, 5, nexus.ScopeProjectGroup, "pg_mes", "MES 专项组",
		nexus.OrgMember{UserID: "u_zhang"})
	syncer, store := newTestSyncer()

	for i := 0; i < 3; i++ {
		if err := syncer.Handle(context.Background(), ev); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	all, _ := store.ListKnowledgeSpacesByEnterprise(context.Background(), "ent_1")
	if len(all) != 1 {
		t.Fatalf("replay created duplicate spaces: %d", len(all))
	}
}

func TestOrgSyncRequiresAndPersistsTypedParentIdentity(t *testing.T) {
	store := newFakeStore()
	service := NewService(store)
	child := orgEvent(nexus.OrgDepartmentUpserted, 2, nexus.ScopeDepartment, "child", "Child")
	child.Scope.ParentID = "parent"
	if _, _, err := service.EnsureSpaceFromEvent(context.Background(), child); err == nil {
		t.Fatal("untyped parent identity accepted")
	}

	child.Scope.ParentKind = nexus.ScopeCompany
	if _, _, err := service.EnsureSpaceFromEvent(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	binding := store.bindings["ent_1|department|child"]
	if !binding.ParentScopeKind.Valid || binding.ParentScopeKind.String != "company" || !binding.ParentScopeID.Valid || binding.ParentScopeID.String != "parent" {
		t.Fatalf("typed parent identity not persisted: %+v", binding)
	}
}

func TestOrgSyncNewerEventReparentsExistingScope(t *testing.T) {
	store := newFakeStore()
	service := NewService(store)
	child := orgEvent(nexus.OrgDepartmentUpserted, 1, nexus.ScopeDepartment, "child", "Child")
	child.Scope.ParentKind, child.Scope.ParentID = nexus.ScopeCompany, "company-a"
	if _, _, err := service.EnsureSpaceFromEvent(context.Background(), child); err != nil {
		t.Fatal(err)
	}

	child.OrgVersion = 2
	child.Scope.ParentKind, child.Scope.ParentID = nexus.ScopeBusinessUnit, "business-b"
	if _, created, err := service.EnsureSpaceFromEvent(context.Background(), child); err != nil || created {
		t.Fatalf("reparent existing scope: created=%v err=%v", created, err)
	}
	binding := store.bindings["ent_1|department|child"]
	if !binding.ParentScopeKind.Valid || binding.ParentScopeKind.String != "business_unit" || !binding.ParentScopeID.Valid || binding.ParentScopeID.String != "business-b" {
		t.Fatalf("newer reparenting was not persisted: %+v", binding)
	}

	child.Scope.ParentKind, child.Scope.ParentID = nexus.ScopeCompany, "equal-version-replay"
	if _, _, err := service.EnsureSpaceFromEvent(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	binding = store.bindings["ent_1|department|child"]
	if binding.ParentScopeKind.String != "business_unit" || binding.ParentScopeID.String != "business-b" {
		t.Fatalf("equal-version replay changed parent identity: %+v", binding)
	}
}

func TestOrgSyncFailureRetryAndOrderingAreAtomic(t *testing.T) {
	t.Run("post-space failure same-version retry", func(t *testing.T) {
		store := newFakeStore()
		service := NewService(store)
		event := orgEvent(nexus.OrgDepartmentUpserted, 1, nexus.ScopeDepartment, "atomic-child", "Atomic child",
			nexus.OrgMember{UserID: "v1-member", DisplayName: "v1"})
		event.Scope.ParentKind, event.Scope.ParentID = nexus.ScopeCompany, "parent-v1"
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}

		event.OrgVersion = 2
		event.Scope.ParentKind, event.Scope.ParentID = nexus.ScopeBusinessUnit, "parent-v2"
		event.Members = []nexus.OrgMember{{UserID: "v2-member", DisplayName: "v2"}}
		store.failBindingOnce = true
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), event); err == nil {
			t.Fatal("injected post-space failure was not returned")
		}
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), event); err != nil {
			t.Fatalf("same-version retry: %v", err)
		}
		assertAtomicOrgState(t, store, "department:atomic-child", 2, "business_unit", "parent-v2", "v2-member")
	})

	t.Run("initial failure retry", func(t *testing.T) {
		store := newFakeStore()
		store.failBindingOnce = true
		service := NewService(store)
		event := orgEvent(nexus.OrgEmployeeUpserted, 1, nexus.ScopeEmployee, "initial-retry", "Initial retry",
			nexus.OrgMember{UserID: "initial-member", DisplayName: "initial"})
		event.Scope.ParentKind, event.Scope.ParentID = nexus.ScopeDepartment, "parent"
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), event); err == nil {
			t.Fatal("injected initial binding failure was not returned")
		}
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), event); err != nil {
			t.Fatalf("initial same-version retry: %v", err)
		}
		assertAtomicOrgState(t, store, "employee:initial-retry", 1, "department", "parent", "initial-member")
	})

	t.Run("concurrent v2 v3", func(t *testing.T) {
		store := newFakeStore()
		service := NewService(store)
		base := orgEvent(nexus.OrgDepartmentUpserted, 1, nexus.ScopeDepartment, "concurrent-child", "Concurrent child")
		base.Scope.ParentKind, base.Scope.ParentID = nexus.ScopeCompany, "parent-v1"
		if _, _, err := service.EnsureSpaceFromEvent(context.Background(), base); err != nil {
			t.Fatal(err)
		}

		store.bindingBlockParent = "parent-v2"
		store.bindingEntered = make(chan struct{}, 1)
		store.bindingRelease = make(chan struct{})
		v2 := base
		v2.OrgVersion, v2.Scope.ParentKind, v2.Scope.ParentID = 2, nexus.ScopeBusinessUnit, "parent-v2"
		v2.Members = []nexus.OrgMember{{UserID: "v2-member", DisplayName: "v2"}}
		v3 := base
		v3.OrgVersion, v3.Scope.ParentKind, v3.Scope.ParentID = 3, nexus.ScopeCompany, "parent-v3"
		v3.Members = []nexus.OrgMember{{UserID: "v3-member", DisplayName: "v3"}}

		v2Done := make(chan error, 1)
		go func() { _, _, err := service.EnsureSpaceFromEvent(context.Background(), v2); v2Done <- err }()
		<-store.bindingEntered
		v3Done := make(chan error, 1)
		go func() { _, _, err := service.EnsureSpaceFromEvent(context.Background(), v3); v3Done <- err }()
		close(store.bindingRelease)
		if err := <-v2Done; err != nil {
			t.Fatal(err)
		}
		if err := <-v3Done; err != nil {
			t.Fatal(err)
		}
		assertAtomicOrgState(t, store, "department:concurrent-child", 3, "company", "parent-v3", "v3-member")
	})
}

func assertAtomicOrgState(t *testing.T, store *fakeStore, scope string, version int64, parentKind, parentID, memberID string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	space := store.spaces[scopeKey("ent_1", scope)]
	binding := store.bindings["ent_1|"+space.Kind+"|"+strings.TrimPrefix(scope, space.Kind+":")]
	if space.OrgVersion != version || !binding.ParentScopeKind.Valid || binding.ParentScopeKind.String != parentKind || !binding.ParentScopeID.Valid || binding.ParentScopeID.String != parentID {
		t.Fatalf("inconsistent space/binding: space=%+v binding=%+v", space, binding)
	}
	member, ok := store.members[space.ID+"|"+memberID]
	if !ok || member.OrgVersion != version {
		t.Fatalf("inconsistent member state: member=%+v found=%v", member, ok)
	}
	memberCount := 0
	for key := range store.members {
		if strings.HasPrefix(key, space.ID+"|") {
			memberCount++
		}
	}
	if memberCount != 1 {
		t.Fatalf("stale or partial members remain: %+v", store.members)
	}
	versionCount := 0
	for _, item := range store.versions {
		if item.SpaceID == space.ID {
			versionCount++
		}
	}
	if versionCount != int(version) {
		t.Fatalf("version snapshots=%d want=%d: %+v", versionCount, version, store.versions)
	}
}
