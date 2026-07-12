package spaces

import (
	"context"
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
	enterprises map[string]db.Enterprise
	spaces      map[string]db.KnowledgeSpace // key enterprise|org_scope
	byID        map[string]db.KnowledgeSpace
	versions    []db.InsertKnowledgeSpaceVersionParams
	bindings    map[string]db.UpsertOrgScopeBindingParams // key enterprise|kind|scope_id
	snapshots   map[string]int64                          // enterprise -> max version seen
	members     map[string]db.UpsertSpaceMemberParams     // key space|user
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
	if s, ok := f.spaces[scopeKey(arg.EnterpriseID, arg.OrgScope)]; ok {
		return s, nil
	}
	return db.KnowledgeSpace{}, pgx.ErrNoRows
}

func (f *fakeStore) InsertKnowledgeSpace(_ context.Context, arg db.InsertKnowledgeSpaceParams) (db.KnowledgeSpace, error) {
	s := db.KnowledgeSpace{
		ID: arg.ID, EnterpriseID: arg.EnterpriseID, Kind: arg.Kind,
		Name: arg.Name, OrgScope: arg.OrgScope, OrgVersion: arg.OrgVersion,
	}
	f.spaces[scopeKey(arg.EnterpriseID, arg.OrgScope)] = s
	f.byID[arg.ID] = s
	return s, nil
}

func (f *fakeStore) UpdateKnowledgeSpaceIfNewer(_ context.Context, arg db.UpdateKnowledgeSpaceIfNewerParams) (int64, error) {
	s, ok := f.byID[arg.ID]
	if !ok || s.OrgVersion > arg.OrgVersion {
		return 0, nil
	}
	s.Name = arg.Name
	s.OrgVersion = arg.OrgVersion
	f.byID[arg.ID] = s
	f.spaces[scopeKey(s.EnterpriseID, s.OrgScope)] = s
	return 1, nil
}

func (f *fakeStore) InsertKnowledgeSpaceVersion(_ context.Context, arg db.InsertKnowledgeSpaceVersionParams) error {
	f.versions = append(f.versions, arg)
	return nil
}

func (f *fakeStore) UpsertOrgScopeBinding(_ context.Context, arg db.UpsertOrgScopeBindingParams) error {
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
	f.members[arg.SpaceID+"|"+arg.UserID] = arg
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
