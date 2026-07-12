package dream

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var inputWindowStart = time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
var inputWindowEnd = time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)

type fakeInputStore struct {
	space          db.KnowledgeSpace
	members        []db.SpaceMembershipCache
	briefs         []db.WorkBrief
	children       []db.ListDreamImmediateChildrenRow
	childRuns      []db.ListDreamCompletedChildRunsRow
	childrenParams []db.ListDreamImmediateChildrenParams
	childRunParams []db.ListDreamCompletedChildRunsParams
	summaries      map[string][]db.DreamSummary
	timeline       []db.TimelineNode
	rawBriefReads  int
	timelineParams []db.ListDreamTimelineNodesParams
	briefParams    []db.ListDreamWorkBriefsForWindowParams
}

func (f *fakeInputStore) GetKnowledgeSpaceByScope(context.Context, db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error) {
	return f.space, nil
}

func (f *fakeInputStore) ListDreamSpaceMembers(context.Context, db.ListDreamSpaceMembersParams) ([]db.SpaceMembershipCache, error) {
	return f.members, nil
}

func (f *fakeInputStore) ListDreamWorkBriefsForWindow(_ context.Context, arg db.ListDreamWorkBriefsForWindowParams) ([]db.WorkBrief, error) {
	f.rawBriefReads++
	f.briefParams = append(f.briefParams, arg)
	return f.briefs, nil
}

func (f *fakeInputStore) ListDreamImmediateChildren(_ context.Context, arg db.ListDreamImmediateChildrenParams) ([]db.ListDreamImmediateChildrenRow, error) {
	f.childrenParams = append(f.childrenParams, arg)
	return f.children, nil
}

func (f *fakeInputStore) ListDreamCompletedChildRuns(_ context.Context, arg db.ListDreamCompletedChildRunsParams) ([]db.ListDreamCompletedChildRunsRow, error) {
	f.childRunParams = append(f.childRunParams, arg)
	return f.childRuns, nil
}

func (f *fakeInputStore) GetDreamSummaryForRunLayer(_ context.Context, arg db.GetDreamSummaryForRunLayerParams) (db.DreamSummary, error) {
	for _, item := range f.summaries[arg.SpaceID+"|"+arg.Layer] {
		if item.EnterpriseID == arg.EnterpriseID && item.RunID == arg.RunID && item.SpaceID == arg.SpaceID && item.Layer == arg.Layer {
			return item, nil
		}
	}
	return db.DreamSummary{}, pgx.ErrNoRows
}

func (f *fakeInputStore) ListDreamTimelineNodes(_ context.Context, arg db.ListDreamTimelineNodesParams) ([]db.TimelineNode, error) {
	f.timelineParams = append(f.timelineParams, arg)
	return f.timeline, nil
}

func TestInputResolverDefaultsParentToImmediateChildSummaries(t *testing.T) {
	store := &fakeInputStore{
		space: db.KnowledgeSpace{ID: "company-space", EnterpriseID: "ent-1", Kind: "company", OrgScope: "company"},
		children: []db.ListDreamImmediateChildrenRow{
			immediateChild("child-c", "dept-c", "company"),
			immediateChild("child-a", "dept-a", "company"),
			immediateChild("child-b", "dept-b", "company"),
		},
		childRuns: []db.ListDreamCompletedChildRunsRow{
			completedChildRun("run-b", "child-b", "dept-b", "company", []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company","shared"]}`)),
			completedChildRun("run-a", "child-a", "dept-a", "company", []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company"]}`)),
		},
		summaries: map[string][]db.DreamSummary{
			"child-a|retrieval": {{ID: "sum-a", RunID: "run-a", EnterpriseID: "ent-1", SpaceID: "child-a", Layer: "retrieval", SummaryText: "A secret-123", EvidencePointerID: pgtype.Text{String: "ev-a", Valid: true}}},
			"child-b|display":   {{ID: "sum-b", RunID: "run-b", EnterpriseID: "ent-1", SpaceID: "child-b", Layer: "display", SummaryText: "B safe", EvidencePointerID: pgtype.Text{String: "ev-b", Valid: true}}},
		},
	}

	inputs, coverage, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
		Visibility: []string{"company", "company_sanitized", "managers"}, MaskingRules: []string{`secret-\d+`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceTypes(inputs); !reflect.DeepEqual(got, []string{"child_dream_summary", "child_dream_summary"}) {
		t.Fatalf("source types = %v", got)
	}
	if store.rawBriefReads != 0 {
		t.Fatal("company rollup read employee briefs")
	}
	if coverage.ExpectedChildren != 3 || coverage.CompletedChildren != 2 || coverage.InputCount != 2 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if got := []string{inputs[0].SourceID, inputs[1].SourceID}; !reflect.DeepEqual(got, []string{"sum-a", "sum-b"}) {
		t.Fatalf("deterministic inputs = %v", got)
	}
	if !strings.HasPrefix(inputs[0].SanitizedText, "A ") || strings.Contains(inputs[0].SanitizedText, "secret-123") || inputs[0].ParentRunID != "run-a" || inputs[0].EvidencePointerID != "ev-a" {
		t.Fatalf("sanitized child input = %+v", inputs[0])
	}
	if !reflect.DeepEqual(inputs[0].Visibility, []string{"company", "company_sanitized"}) {
		t.Fatalf("converged visibility = %v", inputs[0].Visibility)
	}
	if !reflect.DeepEqual(missing, []MissingInput{{SourceType: sdkdream.SourceChildDreamSummary, SourceID: "dept-c", Reason: sdkdream.MissingNotCompleted}}) {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestInputResolverRejectsChildSummaryWithoutVisibilityIntersection(t *testing.T) {
	store := &fakeInputStore{
		children:  []db.ListDreamImmediateChildrenRow{immediateChild("child-a", "dept-a", "company")},
		childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun("run-a", "child-a", "dept-a", "company", []byte(`{"visibility_level":"members","org_unit_ids":["company"]}`))},
		summaries: map[string][]db.DreamSummary{"child-a|retrieval": {{ID: "sum-a", RunID: "run-a", EnterpriseID: "ent-1", SpaceID: "child-a", Layer: "retrieval", SummaryText: "never return me"}}},
	}
	inputs, coverage, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company", "company_sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || coverage.InputCount != 0 {
		t.Fatalf("unauthorized input escaped: %+v coverage=%+v", inputs, coverage)
	}
	if !reflect.DeepEqual(missing, []MissingInput{{SourceType: sdkdream.SourceChildDreamSummary, SourceID: "dept-a", Reason: sdkdream.MissingNotAuthorized}}) {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestInputResolverWorkBriefsAreDirectScopedMaskedAndDeduplicated(t *testing.T) {
	store := &fakeInputStore{
		space:   db.KnowledgeSpace{ID: "project-space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: "project-7"},
		members: []db.SpaceMembershipCache{{SpaceID: "project-space", UserID: "u2"}, {SpaceID: "project-space", UserID: "u1"}},
		briefs: []db.WorkBrief{
			{ID: "brief-b", EnterpriseID: "ent-1", EmployeeUserID: "u2", BriefDate: dreamDate(inputWindowStart), Summary: "done token-9", EvidencePointerID: "ev-b"},
			{ID: "brief-a", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(inputWindowStart), Summary: "built token-8", EvidencePointerID: "ev-a"},
			{ID: "brief-a", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(inputWindowStart), Summary: "duplicate raw", EvidencePointerID: "ev-a"},
			{ID: "foreign", EnterpriseID: "ent-2", EmployeeUserID: "u1", BriefDate: dreamDate(inputWindowStart), Summary: "foreign raw"},
			{ID: "not-member", EnterpriseID: "ent-1", EmployeeUserID: "u3", BriefDate: dreamDate(inputWindowStart), Summary: "outsider raw"},
		},
	}
	inputs, coverage, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "project-7", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief, sdkdream.SourceWorkBrief},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"project-7"}, MaskingRules: []string{`token-\d+`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 || coverage.InputCount != 2 {
		t.Fatalf("coverage=%+v missing=%+v", coverage, missing)
	}
	if got := []string{inputs[0].SourceID, inputs[1].SourceID}; !reflect.DeepEqual(got, []string{"brief-a", "brief-b"}) {
		t.Fatalf("inputs = %v", got)
	}
	if strings.Contains(inputs[0].SanitizedText, "token-8") || strings.Contains(inputs[1].SanitizedText, "token-9") {
		t.Fatalf("unmasked briefs = %+v", inputs)
	}
	if store.rawBriefReads != 1 {
		t.Fatalf("duplicate source caused %d raw reads", store.rawBriefReads)
	}
}

func TestInputResolverNeverReadsParentWorkBriefs(t *testing.T) {
	store := &fakeInputStore{space: db.KnowledgeSpace{ID: "company-space", EnterpriseID: "ent-1", Kind: "company", OrgScope: "company"}}
	inputs, _, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || store.rawBriefReads != 0 {
		t.Fatalf("parent raw access: inputs=%+v reads=%d", inputs, store.rawBriefReads)
	}
	if !reflect.DeepEqual(missing, []MissingInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: "company", Reason: sdkdream.MissingNotAuthorized}}) {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestInputResolverTimelineSourcesAreTypedWindowedScopedAndMasked(t *testing.T) {
	store := &fakeInputStore{
		space: db.KnowledgeSpace{ID: "space-1", EnterpriseID: "ent-1", Kind: "department", OrgScope: "dept-a"},
		timeline: []db.TimelineNode{
			{ID: "risk-b", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "risk credential-2", EvidencePointerID: pgtype.Text{String: "ev-risk", Valid: true}},
			{ID: "project-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "project_record", SummaryText: "project credential-1"},
			{ID: "sop-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "sop_update", SummaryText: "sop credential-3"},
			{ID: "answer-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "agent_answer", SummaryText: "answer credential-4"},
			{ID: "external-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "external_evidence", SummaryText: "external credential-5"},
			{ID: "task-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "completed_task", SummaryText: "task credential-6"},
			{ID: "project-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "project_record", SummaryText: "duplicate raw"},
			{ID: "wrong-type", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "dream_summary", SummaryText: "wrong type"},
			{ID: "foreign", EnterpriseID: "ent-2", SpaceID: "space-1", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "foreign"},
			{ID: "wrong-space", EnterpriseID: "ent-1", SpaceID: "space-2", OrgScope: "dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "wrong space"},
			{ID: "wrong-org", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-b", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "wrong org"},
		},
	}
	inputs, coverage, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "dept-a", Sources: []sdkdream.Source{
			sdkdream.SourceRiskEvent, sdkdream.SourceProjectRecord, sdkdream.SourceSOPUpdate,
			sdkdream.SourceAgentAnswer, sdkdream.SourceExternalEvidence, sdkdream.SourceCompletedTask,
		},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"dept-a"}, MaskingRules: []string{`credential-\d+`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 || coverage.InputCount != 6 {
		t.Fatalf("coverage=%+v missing=%+v", coverage, missing)
	}
	if got := sourceTypes(inputs); !reflect.DeepEqual(got, []string{"agent_answer", "completed_task", "external_evidence", "project_record", "risk_event", "sop_update"}) {
		t.Fatalf("types = %v", got)
	}
	for _, input := range inputs {
		if strings.Contains(input.SanitizedText, "credential-") {
			t.Fatalf("timeline text = %+v", inputs)
		}
	}
	if len(store.timelineParams) != 6 {
		t.Fatalf("timeline calls = %d", len(store.timelineParams))
	}
	for _, arg := range store.timelineParams {
		if arg.EnterpriseID != "ent-1" || arg.SpaceID != "space-1" || arg.OrgScope != "dept-a" || !arg.WindowStart.Valid || !arg.WindowStart.Time.Equal(inputWindowStart) || !arg.WindowEnd.Valid || !arg.WindowEnd.Time.Equal(inputWindowEnd) || arg.ResultLimit <= 0 {
			t.Fatalf("unbounded timeline query = %+v", arg)
		}
	}
}

type staticSourceResolver struct {
	inputs         []SourceInput
	coverage       Coverage
	missing        []MissingInput
	preserveMasked bool
}

func (s staticSourceResolver) ResolveSource(_ context.Context, _ ResolveRequest, masker *Masker) ([]SourceInput, Coverage, []MissingInput, error) {
	inputs := append([]SourceInput(nil), s.inputs...)
	if !s.preserveMasked {
		for i := range inputs {
			if inputs[i].MaskedText.owner == nil {
				inputs[i].MaskedText = masker.Sanitize(inputs[i].MaskedText.text)
			}
		}
	}
	if s.coverage == (Coverage{}) {
		s.coverage.InputCount = len(inputs)
	}
	return inputs, s.coverage, s.missing, nil
}

func TestInputResolverSupportsPluggableSourcesAndBoundsOutput(t *testing.T) {
	const custom sdkdream.Source = "custom_sanitized"
	r := NewInputResolver(&fakeInputStore{})
	if err := r.Register(custom, staticSourceResolver{inputs: []SourceInput{
		pluginSourceInput(custom, "z", "z safe", []string{"company"}),
		pluginSourceInput(custom, "a", "a", []string{"private"}),
		pluginSourceInput(custom, "a", "duplicate", []string{"private"}),
	}}); err != nil {
		t.Fatal(err)
	}
	inputs, coverage, _, err := r.Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
		Sources: []sdkdream.Source{custom}, MaxInputs: 3, Visibility: []string{"company"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].SourceType != custom || inputs[0].SourceID != "z" || coverage.InputCount != 1 {
		t.Fatalf("bounded inputs=%+v coverage=%+v", inputs, coverage)
	}
}

func sourceTypes(inputs []ResolvedInput) []string {
	got := make([]string, len(inputs))
	for i, input := range inputs {
		got[i] = string(input.SourceType)
	}
	return got
}

func dreamTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func dreamDate(value time.Time) pgtype.Date {
	return pgtype.Date{Time: value, Valid: true}
}

func immediateChild(id, orgScope, parent string) db.ListDreamImmediateChildrenRow {
	parentScope := parent
	parentKind := "company"
	if parsed, ok := parseScopeRef(parent); ok && parsed.kind != "" {
		parentKind, parent = parsed.kind, parsed.id
	} else {
		parentScope = "company:" + parent
	}
	return db.ListDreamImmediateChildrenRow{
		ID: id, EnterpriseID: "ent-1", OrgScope: orgScope, ParentSpaceID: "company-space",
		ParentScopeKind: parentKind, ParentScopeID: parent, ParentOrgScope: parentScope,
	}
}

func completedChildRun(id, childSpaceID, childOrgScope, parent string, visibility []byte) db.ListDreamCompletedChildRunsRow {
	parentScope := parent
	parentKind := "company"
	if parsed, ok := parseScopeRef(parent); ok && parsed.kind != "" {
		parentKind, parent = parsed.kind, parsed.id
	} else {
		parentScope = "company:" + parent
	}
	return db.ListDreamCompletedChildRunsRow{
		ID: id, EnterpriseID: "ent-1", OrgUnitID: childOrgScope, Status: "succeeded",
		WindowStart: dreamTimestamp(inputWindowStart), WindowEnd: dreamTimestamp(inputWindowEnd), VisibilitySnapshot: visibility,
		ChildSpaceID: childSpaceID, ChildOrgScope: childOrgScope, ParentSpaceID: "company-space",
		ParentScopeKind: parentKind, ParentScopeID: parent, ParentOrgScope: parentScope,
	}
}

func pluginSourceInput(source sdkdream.Source, id, text string, visibility []string) SourceInput {
	return SourceInput{
		SourceType: source, SourceID: id, OrgUnitID: "company", MaskedText: MaskedText{text: text}, Visibility: visibility,
		EnterpriseID: "ent-1", SpaceID: "company-space", WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
	}
}

func TestInputResolverRejectsZeroOrHalfOpenWindows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "both zero"},
		{name: "missing start", end: inputWindowEnd},
		{name: "missing end", start: inputWindowStart},
		{name: "equal endpoints", start: inputWindowStart, end: inputWindowStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewInputResolver(&fakeInputStore{})
			if err := resolver.Register("custom", staticSourceResolver{}); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := resolver.Resolve(context.Background(), ResolveRequest{
				EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{"custom"},
				WindowStart: tc.start, WindowEnd: tc.end, Visibility: []string{"company"},
			})
			if err == nil {
				t.Fatal("incomplete/non-positive input window accepted")
			}
		})
	}
}

func TestInputResolverRejectsOutOfWindowRows(t *testing.T) {
	t.Run("timeline", func(t *testing.T) {
		store := &fakeInputStore{
			space: db.KnowledgeSpace{ID: "space-1", EnterpriseID: "ent-1", Kind: "department", OrgScope: "department:dept-a"},
			timeline: []db.TimelineNode{{
				ID: "late", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "department:dept-a",
				NodeTime: dreamTimestamp(inputWindowEnd.Add(time.Second)), SourceType: "risk_event", SummaryText: "late",
			}},
		}
		inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "department:dept-a", Sources: []sdkdream.Source{sdkdream.SourceRiskEvent},
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"department:dept-a"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 0 {
			t.Fatalf("out-of-window timeline row accepted: %+v", inputs)
		}
	})

	t.Run("brief", func(t *testing.T) {
		store := &fakeInputStore{
			space:   db.KnowledgeSpace{ID: "space-1", EnterpriseID: "ent-1", Kind: "employee", OrgScope: "employee:u1"},
			members: []db.SpaceMembershipCache{{SpaceID: "space-1", UserID: "u1"}},
			briefs: []db.WorkBrief{{
				ID: "late", EnterpriseID: "ent-1", EmployeeUserID: "u1",
				BriefDate: pgtype.Date{Time: inputWindowEnd.AddDate(0, 0, 1), Valid: true}, Summary: "late",
			}},
		}
		inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "employee:u1", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"employee:u1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 0 {
			t.Fatalf("out-of-window brief accepted: %+v", inputs)
		}
	})
}

func TestInputResolverEmptyVisibilityFailsClosed(t *testing.T) {
	store := &fakeInputStore{
		space:    db.KnowledgeSpace{ID: "space-1", EnterpriseID: "ent-1", Kind: "department", OrgScope: "department:dept-a"},
		timeline: []db.TimelineNode{{ID: "risk", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "department:dept-a", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "risk"}},
	}
	inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "department:dept-a", Sources: []sdkdream.Source{sdkdream.SourceRiskEvent},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
	})
	if err == nil || len(inputs) != 0 {
		t.Fatalf("empty visibility accepted: inputs=%+v err=%v", inputs, err)
	}
}

func TestInputResolverMalformedChildVisibilityFailsClosed(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{broken`), []byte(`{"visibility_level":"unknown","org_unit_ids":["company"]}`), []byte(`{"visibility_level":"members","org_unit_ids":[]}`)} {
		store := &fakeInputStore{
			children:  []db.ListDreamImmediateChildrenRow{immediateChild("child", "department:child", "company")},
			childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun("run", "child", "department:child", "company", raw)},
			summaries: map[string][]db.DreamSummary{"child|retrieval": {{ID: "sum", RunID: "run", EnterpriseID: "ent-1", SpaceID: "child", Layer: "retrieval", SummaryText: "private"}}},
		}
		inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company", "members"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 0 {
			t.Fatalf("invalid visibility snapshot %q accepted: %+v", raw, inputs)
		}
	}
}

func TestInputResolverRejectsInjectedNonChild(t *testing.T) {
	store := &fakeInputStore{
		children:  []db.ListDreamImmediateChildrenRow{{ID: "grandchild", EnterpriseID: "ent-1", OrgScope: "department:grandchild"}},
		childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun("run", "grandchild", "department:grandchild", "company", []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company"]}`))},
		summaries: map[string][]db.DreamSummary{"grandchild|retrieval": {{ID: "sum", RunID: "run", EnterpriseID: "ent-1", SpaceID: "grandchild", Layer: "retrieval", SummaryText: "grandchild"}}},
	}
	inputs, coverage, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company", "company_sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || coverage.ExpectedChildren != 0 {
		t.Fatalf("unverifiable child accepted: inputs=%+v coverage=%+v", inputs, coverage)
	}
}

func TestInputResolverProtectsBuiltinsAndPluginContract(t *testing.T) {
	t.Run("builtin replacement", func(t *testing.T) {
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(sdkdream.SourceWorkBrief, staticSourceResolver{}); err == nil {
			t.Fatal("security-critical builtin was replaceable")
		}
	})

	t.Run("unmasked plugin", func(t *testing.T) {
		const custom sdkdream.Source = "custom"
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, staticSourceResolver{inputs: []SourceInput{pluginSourceInput(custom, "raw", "secret-123", []string{"company"})}, preserveMasked: true}); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom}, WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
			Visibility: []string{"company"}, MaskingRules: []string{`secret-\d+`},
		})
		if err == nil {
			t.Fatal("unmasked plugin output was repaired instead of rejected")
		}
	})

	t.Run("foreign masker provenance", func(t *testing.T) {
		const custom sdkdream.Source = "custom"
		foreignMasker, err := NewMasker([]string{`secret-\d+`})
		if err != nil {
			t.Fatal(err)
		}
		item := pluginSourceInput(custom, "foreign-masker", "", []string{"company"})
		item.MaskedText = foreignMasker.Sanitize("secret-123")
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, staticSourceResolver{inputs: []SourceInput{item}, preserveMasked: true}); err != nil {
			t.Fatal(err)
		}
		_, _, _, err = r.Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom}, WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
			Visibility: []string{"company"}, MaskingRules: []string{`secret-\d+`},
		})
		if err == nil {
			t.Fatal("text sanitized by a different masker instance was accepted")
		}
	})

	t.Run("spoofed metadata", func(t *testing.T) {
		const custom sdkdream.Source = "custom"
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, staticSourceResolver{
			coverage: Coverage{ExpectedChildren: -1, CompletedChildren: 999999, InputCount: -1},
			missing:  []MissingInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: "x", Reason: "made_up"}},
		}); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom}, WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
		})
		if err == nil {
			t.Fatal("spoofed plugin coverage/missing metadata accepted")
		}
	})
}

func TestInputResolverRejectsOversizedPluginBeforeAccumulation(t *testing.T) {
	const custom sdkdream.Source = "custom"
	items := make([]SourceInput, maxResolvedInputs+1)
	for i := range items {
		items[i] = pluginSourceInput(custom, fmt.Sprintf("id-%04d", i), "safe", []string{"company"})
	}
	r := NewInputResolver(&fakeInputStore{})
	if err := r.Register(custom, staticSourceResolver{inputs: items}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom}, WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
	})
	if err == nil {
		t.Fatal("oversized plugin result accepted for later truncation")
	}
}

func TestInputResolverRejectsAmbiguousCanonicalScope(t *testing.T) {
	store := &fakeInputStore{
		space:   db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: "evil:project-7"},
		members: []db.SpaceMembershipCache{{SpaceID: "space", UserID: "u1"}},
		briefs:  []db.WorkBrief{{ID: "brief", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: pgtype.Date{Time: inputWindowStart, Valid: true}, Summary: "raw"}},
	}
	inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "project-7", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"project-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || store.rawBriefReads != 0 {
		t.Fatalf("ambiguous canonical scope accepted: inputs=%+v reads=%d", inputs, store.rawBriefReads)
	}
}

func TestInputResolverSelectsExactRunSummary(t *testing.T) {
	distractors := make([]db.DreamSummary, defaultMaxResolvedInputs+1)
	for i := 0; i < defaultMaxResolvedInputs; i++ {
		distractors[i] = db.DreamSummary{ID: fmt.Sprintf("d-%04d", i), RunID: fmt.Sprintf("other-%04d", i), EnterpriseID: "ent-1", SpaceID: "child", Layer: "retrieval", SummaryText: "other"}
	}
	distractors[defaultMaxResolvedInputs] = db.DreamSummary{ID: "target", RunID: "run", EnterpriseID: "ent-1", SpaceID: "child", Layer: "retrieval", SummaryText: "target"}
	store := &fakeInputStore{
		children:  []db.ListDreamImmediateChildrenRow{immediateChild("child", "department:child", "company")},
		childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun("run", "child", "department:child", "company", []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company"]}`))},
		summaries: map[string][]db.DreamSummary{"child|retrieval": distractors},
	}
	inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company", "company_sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].SourceID != "target" {
		t.Fatalf("exact-run summary not selected: %+v", inputs)
	}
}

func TestInputResolverDuplicateChoiceIsDeterministic(t *testing.T) {
	const custom sdkdream.Source = "custom"
	resolve := func(items []SourceInput) string {
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, staticSourceResolver{inputs: items}); err != nil {
			t.Fatal(err)
		}
		inputs, _, _, err := r.Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom}, WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
		})
		if err != nil || len(inputs) != 1 {
			t.Fatalf("resolve duplicate: inputs=%+v err=%v", inputs, err)
		}
		return inputs[0].SanitizedText
	}
	a := pluginSourceInput(custom, "same", "a", []string{"company"})
	b := pluginSourceInput(custom, "same", "b", []string{"company"})
	if first, second := resolve([]SourceInput{b, a}), resolve([]SourceInput{a, b}); first != second || first != "a" {
		t.Fatalf("duplicate choice depends on arrival: first=%q second=%q", first, second)
	}
}

func TestInputResolverValidatesPluginProvenanceAndOwnership(t *testing.T) {
	const custom sdkdream.Source = "custom"
	baseRequest := ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
	}
	assertRejected := func(t *testing.T, source SourceResolver, req ResolveRequest) {
		t.Helper()
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, source); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Resolve(context.Background(), req); err == nil {
			t.Fatal("invalid plugin result accepted")
		}
	}

	t.Run("source spoof", func(t *testing.T) {
		item := pluginSourceInput(sdkdream.SourceWorkBrief, "id", "safe", []string{"company"})
		assertRejected(t, staticSourceResolver{inputs: []SourceInput{item}}, baseRequest)
	})

	for _, tc := range []struct {
		name   string
		mutate func(*SourceInput)
	}{
		{name: "enterprise", mutate: func(input *SourceInput) { input.EnterpriseID = "ent-2" }},
		{name: "org", mutate: func(input *SourceInput) { input.OrgUnitID = "other" }},
		{name: "space", mutate: func(input *SourceInput) { input.SpaceID = "other-space" }},
		{name: "window start", mutate: func(input *SourceInput) { input.WindowStart = input.WindowStart.Add(time.Second) }},
		{name: "window end", mutate: func(input *SourceInput) { input.WindowEnd = input.WindowEnd.Add(time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := pluginSourceInput(custom, "id", "safe", []string{"company"})
			tc.mutate(&item)
			assertRejected(t, staticSourceResolver{inputs: []SourceInput{item}}, baseRequest)
		})
	}

	for _, missing := range []MissingInput{
		{SourceType: sdkdream.SourceWorkBrief, SourceID: "id", Reason: sdkdream.MissingNotFound},
		{SourceType: custom, SourceID: "", Reason: sdkdream.MissingNotFound},
		{SourceType: custom, SourceID: "id", Reason: "invented"},
	} {
		t.Run("missing "+string(missing.SourceType)+" "+string(missing.Reason), func(t *testing.T) {
			assertRejected(t, staticSourceResolver{missing: []MissingInput{missing}}, baseRequest)
		})
	}

	t.Run("empty source visibility", func(t *testing.T) {
		r := NewInputResolver(&fakeInputStore{})
		if err := r.Register(custom, staticSourceResolver{inputs: []SourceInput{pluginSourceInput(custom, "id", "safe", nil)}}); err != nil {
			t.Fatal(err)
		}
		inputs, _, missing, err := r.Resolve(context.Background(), baseRequest)
		if err == nil || len(inputs) != 0 || len(missing) != 0 {
			t.Fatalf("empty source visibility did not fail closed: inputs=%+v missing=%+v", inputs, missing)
		}
	})
}

func TestInputResolverBoundsBuiltinsBeforeAccumulation(t *testing.T) {
	t.Run("members", func(t *testing.T) {
		store := &fakeInputStore{space: db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "employee", OrgScope: "employee:u1"}}
		for _, id := range []string{"u1", "u2", "u3"} {
			store.members = append(store.members, db.SpaceMembershipCache{SpaceID: "space", UserID: id})
		}
		_, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "employee:u1", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief}, MaxInputs: 2,
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"employee:u1"},
		})
		if err == nil || store.rawBriefReads != 0 {
			t.Fatalf("oversized members accumulated: reads=%d err=%v", store.rawBriefReads, err)
		}
	})

	t.Run("briefs", func(t *testing.T) {
		store := &fakeInputStore{
			space:   db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "employee", OrgScope: "employee:u1"},
			members: []db.SpaceMembershipCache{{SpaceID: "space", UserID: "u1"}},
		}
		for i := 0; i < 3; i++ {
			store.briefs = append(store.briefs, db.WorkBrief{ID: fmt.Sprintf("brief-%d", i), EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(inputWindowStart), Summary: "safe"})
		}
		_, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "employee:u1", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief}, MaxInputs: 2,
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"employee:u1"},
		})
		if err == nil {
			t.Fatal("oversized briefs accumulated")
		}
	})

	t.Run("timeline", func(t *testing.T) {
		store := &fakeInputStore{space: db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "department", OrgScope: "department:d1"}}
		for i := 0; i < 3; i++ {
			store.timeline = append(store.timeline, db.TimelineNode{ID: fmt.Sprintf("node-%d", i), EnterpriseID: "ent-1", SpaceID: "space", OrgScope: "department:d1", NodeTime: dreamTimestamp(inputWindowStart), SourceType: "risk_event", SummaryText: "safe"})
		}
		_, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "department:d1", Sources: []sdkdream.Source{sdkdream.SourceRiskEvent}, MaxInputs: 2,
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"department:d1"},
		})
		if err == nil {
			t.Fatal("oversized timeline accumulated")
		}
	})

	t.Run("children", func(t *testing.T) {
		store := &fakeInputStore{}
		for i := 0; i < 3; i++ {
			store.children = append(store.children, immediateChild(fmt.Sprintf("child-%d", i), fmt.Sprintf("department:d%d", i), "company"))
		}
		_, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary}, MaxInputs: 2,
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
		})
		if err == nil {
			t.Fatal("oversized children accumulated")
		}
	})

	t.Run("child runs", func(t *testing.T) {
		store := &fakeInputStore{children: []db.ListDreamImmediateChildrenRow{immediateChild("child", "department:d1", "company")}}
		for i := 0; i < 3; i++ {
			store.childRuns = append(store.childRuns, completedChildRun(fmt.Sprintf("run-%d", i), "child", "department:d1", "company", []byte(`{"visibility_level":"members","org_unit_ids":["company"]}`)))
		}
		_, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
			EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary}, MaxInputs: 2,
			WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company", "members"},
		})
		if err == nil {
			t.Fatal("oversized child runs accumulated")
		}
	})
}

func TestInputResolverRejectsReservedVisibilityLevelAsChildOrg(t *testing.T) {
	store := &fakeInputStore{
		children: []db.ListDreamImmediateChildrenRow{immediateChild("child", "department:child", "company")},
		childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun(
			"run", "child", "department:child", "company",
			[]byte(`{"visibility_level":"members","org_unit_ids":["managers"]}`),
		)},
		summaries: map[string][]db.DreamSummary{
			"child|retrieval": {{ID: "sum", RunID: "run", EnterpriseID: "ent-1", SpaceID: "child", Layer: "retrieval", SummaryText: "private"}},
		},
	}
	inputs, _, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", OrgUnitKind: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"members", "managers"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || len(missing) != 1 || missing[0].Reason != sdkdream.MissingNotAuthorized {
		t.Fatalf("reserved visibility token accepted as org scope: inputs=%+v missing=%+v", inputs, missing)
	}
}

func TestInputResolverBareParentUsesResolvedScopeKind(t *testing.T) {
	store := &fakeInputStore{
		space:    db.KnowledgeSpace{ID: "company-collision", EnterpriseID: "ent-1", Kind: "company", OrgScope: "collision"},
		children: []db.ListDreamImmediateChildrenRow{immediateChild("wrong-child", "employee:wrong", "department:collision")},
		childRuns: []db.ListDreamCompletedChildRunsRow{completedChildRun(
			"wrong-run", "wrong-child", "employee:wrong", "department:collision",
			[]byte(`{"visibility_level":"company_sanitized","org_unit_ids":["collision"]}`),
		)},
		summaries: map[string][]db.DreamSummary{
			"wrong-child|retrieval": {{ID: "wrong-summary", RunID: "wrong-run", EnterpriseID: "ent-1", SpaceID: "wrong-child", Layer: "retrieval", SummaryText: "must not leak"}},
		},
	}
	inputs, coverage, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "collision", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"collision", "company_sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 0 || coverage.ExpectedChildren != 0 {
		t.Fatalf("bare company parent crossed into department identity: inputs=%+v coverage=%+v", inputs, coverage)
	}
	if len(store.childrenParams) != 1 || store.childrenParams[0].ParentScopeKind != "company" || store.childrenParams[0].ParentScopeID != "collision" ||
		len(store.childRunParams) != 1 || store.childRunParams[0].ParentScopeKind != "company" || store.childRunParams[0].ParentScopeID != "collision" {
		t.Fatalf("bare parent query lost exact identity: children=%+v runs=%+v", store.childrenParams, store.childRunParams)
	}
}

func TestInputResolverBroadRegexMaskingDoesNotRequireRemasking(t *testing.T) {
	for _, rule := range []string{".", `\S+`} {
		t.Run(rule, func(t *testing.T) {
			store := &fakeInputStore{
				space:   db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "employee", OrgScope: "employee:u1"},
				members: []db.SpaceMembershipCache{{SpaceID: "space", UserID: "u1"}},
				briefs: []db.WorkBrief{{
					ID: "brief", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(inputWindowStart), Summary: "raw",
				}},
			}
			inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
				EnterpriseID: "ent-1", OrgUnitID: "employee:u1", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
				WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"employee:u1"}, MaskingRules: []string{rule},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 || strings.Contains(inputs[0].SanitizedText, "raw") {
				t.Fatalf("broad-regex masking failed: %+v", inputs)
			}
		})
	}
}

func TestInputResolverRejectsGlobalAggregateOverflow(t *testing.T) {
	r := NewInputResolver(&fakeInputStore{})
	sources := []sdkdream.Source{"custom-a", "custom-b", "custom-c"}
	for _, source := range sources {
		items := []SourceInput{
			pluginSourceInput(source, string(source)+"-1", "safe", []string{"company"}),
			pluginSourceInput(source, string(source)+"-2", "safe", []string{"company"}),
		}
		if err := r.Register(source, staticSourceResolver{inputs: items}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: sources, MaxInputs: 3,
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
	})
	if err == nil {
		t.Fatal("per-source bounds allowed aggregate overflow")
	}
}

func TestInputResolverRejectsRawSourceOverflowBeforeDeduplication(t *testing.T) {
	sources := make([]sdkdream.Source, maxResolvedInputs+1)
	for i := range sources {
		sources[i] = "custom"
	}
	r := NewInputResolver(&fakeInputStore{})
	if err := r.Register("custom", staticSourceResolver{}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: sources,
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
	})
	if err == nil {
		t.Fatal("oversized raw source slice was allocated and deduplicated")
	}
}

func TestInputResolverRegistryIsRaceSafe(t *testing.T) {
	const custom sdkdream.Source = "custom"
	r := NewInputResolver(&fakeInputStore{})
	resolver := func(text string) staticSourceResolver {
		return staticSourceResolver{inputs: []SourceInput{pluginSourceInput(custom, "id", text, []string{"company"})}}
	}
	if err := r.Register(custom, resolver("initial")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := r.Register(custom, resolver(fmt.Sprintf("value-%d", i))); err != nil {
				errs <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			_, _, _, err := r.Resolve(context.Background(), ResolveRequest{
				EnterpriseID: "ent-1", OrgUnitID: "company", SpaceID: "company-space", Sources: []sdkdream.Source{custom},
				WindowStart: inputWindowStart, WindowEnd: inputWindowEnd, Visibility: []string{"company"},
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestInputResolverBriefDateBoundsUsePolicyTimezone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		timezone   string
		start, end time.Time
		wantStart  time.Time
		wantEnd    time.Time
	}{
		{
			name: "Asia Shanghai midnight window", timezone: "Asia/Shanghai",
			start: time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC), end: time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC),
			wantStart: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), wantEnd: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "same local day nonaligned window", timezone: "Asia/Shanghai",
			start: time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC), end: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
			wantStart: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), wantEnd: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "New York spring DST day", timezone: "America/New_York",
			start: time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), end: time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC),
			wantStart: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), wantEnd: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeInputStore{
				space:   db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "employee", OrgScope: "employee:u1"},
				members: []db.SpaceMembershipCache{{SpaceID: "space", UserID: "u1"}},
				briefs: []db.WorkBrief{
					{ID: "inside", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(tc.wantStart), Summary: "inside"},
					{ID: "outside", EnterpriseID: "ent-1", EmployeeUserID: "u1", BriefDate: dreamDate(tc.wantEnd), Summary: "outside"},
				},
			}
			inputs, _, _, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
				EnterpriseID: "ent-1", OrgUnitID: "employee:u1", Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
				WindowStart: tc.start, WindowEnd: tc.end, Timezone: tc.timezone, Visibility: []string{"employee:u1"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 || inputs[0].SourceID != "inside" {
				t.Fatalf("timezone brief selection: %+v", inputs)
			}
			if len(store.briefParams) != 1 || !store.briefParams[0].WindowStart.Time.Equal(tc.wantStart) || !store.briefParams[0].WindowEnd.Time.Equal(tc.wantEnd) {
				t.Fatalf("SQL bounds differ: %+v", store.briefParams)
			}
		})
	}
}
