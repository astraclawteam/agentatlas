package dream

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

var inputWindowStart = time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
var inputWindowEnd = time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)

type fakeInputStore struct {
	space          db.KnowledgeSpace
	members        []db.SpaceMembershipCache
	briefs         []db.WorkBrief
	children       []db.KnowledgeSpace
	childRuns      []db.DreamRun
	summaries      map[string][]db.DreamSummary
	timeline       []db.TimelineNode
	rawBriefReads  int
	timelineParams []db.ListTimelineNodesParams
}

func (f *fakeInputStore) GetKnowledgeSpaceByScope(context.Context, db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error) {
	return f.space, nil
}

func (f *fakeInputStore) ListSpaceMembers(context.Context, string) ([]db.SpaceMembershipCache, error) {
	return f.members, nil
}

func (f *fakeInputStore) ListWorkBriefsForWindow(context.Context, db.ListWorkBriefsForWindowParams) ([]db.WorkBrief, error) {
	f.rawBriefReads++
	return f.briefs, nil
}

func (f *fakeInputStore) ListChildSpaces(context.Context, db.ListChildSpacesParams) ([]db.KnowledgeSpace, error) {
	return f.children, nil
}

func (f *fakeInputStore) ListCompletedChildDreamRuns(context.Context, db.ListCompletedChildDreamRunsParams) ([]db.DreamRun, error) {
	return f.childRuns, nil
}

func (f *fakeInputStore) ListDreamSummariesBySpace(_ context.Context, arg db.ListDreamSummariesBySpaceParams) ([]db.DreamSummary, error) {
	return f.summaries[arg.SpaceID+"|"+arg.Layer], nil
}

func (f *fakeInputStore) ListTimelineNodes(_ context.Context, arg db.ListTimelineNodesParams) ([]db.TimelineNode, error) {
	f.timelineParams = append(f.timelineParams, arg)
	return f.timeline, nil
}

func TestInputResolverDefaultsParentToImmediateChildSummaries(t *testing.T) {
	store := &fakeInputStore{
		space: db.KnowledgeSpace{ID: "company-space", EnterpriseID: "ent-1", Kind: "company", OrgScope: "company"},
		children: []db.KnowledgeSpace{
			{ID: "child-c", EnterpriseID: "ent-1", OrgScope: "dept-c"},
			{ID: "child-a", EnterpriseID: "ent-1", OrgScope: "dept-a"},
			{ID: "child-b", EnterpriseID: "ent-1", OrgScope: "dept-b"},
		},
		childRuns: []db.DreamRun{
			{ID: "run-b", EnterpriseID: "ent-1", OrgUnitID: "dept-b", Status: "succeeded", WindowStart: dreamTimestamp(inputWindowStart), WindowEnd: dreamTimestamp(inputWindowEnd), VisibilitySnapshot: []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company","shared"]}`)},
			{ID: "run-a", EnterpriseID: "ent-1", OrgUnitID: "dept-a", Status: "succeeded", WindowStart: dreamTimestamp(inputWindowStart), WindowEnd: dreamTimestamp(inputWindowEnd), VisibilitySnapshot: []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company"]}`)},
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
		children:  []db.KnowledgeSpace{{ID: "child-a", EnterpriseID: "ent-1", OrgScope: "dept-a"}},
		childRuns: []db.DreamRun{{ID: "run-a", EnterpriseID: "ent-1", OrgUnitID: "dept-a", Status: "succeeded", WindowStart: dreamTimestamp(inputWindowStart), WindowEnd: dreamTimestamp(inputWindowEnd), VisibilitySnapshot: []byte(`{"visibility_level":"members","org_unit_ids":["company"]}`)}},
		summaries: map[string][]db.DreamSummary{"child-a|retrieval": {{ID: "sum-a", RunID: "run-a", EnterpriseID: "ent-1", SpaceID: "child-a", Layer: "retrieval", SummaryText: "never return me"}}},
	}
	inputs, coverage, missing, err := NewInputResolver(store).Resolve(context.Background(), ResolveRequest{
		EnterpriseID: "ent-1", OrgUnitID: "company", Sources: []sdkdream.Source{sdkdream.SourceChildDreamSummary},
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
			{ID: "brief-b", EnterpriseID: "ent-1", EmployeeUserID: "u2", Summary: "done token-9", EvidencePointerID: "ev-b"},
			{ID: "brief-a", EnterpriseID: "ent-1", EmployeeUserID: "u1", Summary: "built token-8", EvidencePointerID: "ev-a"},
			{ID: "brief-a", EnterpriseID: "ent-1", EmployeeUserID: "u1", Summary: "duplicate raw", EvidencePointerID: "ev-a"},
			{ID: "foreign", EnterpriseID: "ent-2", EmployeeUserID: "u1", Summary: "foreign raw"},
			{ID: "not-member", EnterpriseID: "ent-1", EmployeeUserID: "u3", Summary: "outsider raw"},
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
		WindowStart: inputWindowStart, WindowEnd: inputWindowEnd,
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
			{ID: "risk-b", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "risk_event", SummaryText: "risk credential-2", EvidencePointerID: pgtype.Text{String: "ev-risk", Valid: true}},
			{ID: "project-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "project_record", SummaryText: "project credential-1"},
			{ID: "sop-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "sop_update", SummaryText: "sop credential-3"},
			{ID: "answer-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "agent_answer", SummaryText: "answer credential-4"},
			{ID: "external-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "external_evidence", SummaryText: "external credential-5"},
			{ID: "task-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "completed_task", SummaryText: "task credential-6"},
			{ID: "project-a", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "project_record", SummaryText: "duplicate raw"},
			{ID: "wrong-type", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "dream_summary", SummaryText: "wrong type"},
			{ID: "foreign", EnterpriseID: "ent-2", SpaceID: "space-1", OrgScope: "dept-a", SourceType: "risk_event", SummaryText: "foreign"},
			{ID: "wrong-space", EnterpriseID: "ent-1", SpaceID: "space-2", OrgScope: "dept-a", SourceType: "risk_event", SummaryText: "wrong space"},
			{ID: "wrong-org", EnterpriseID: "ent-1", SpaceID: "space-1", OrgScope: "dept-b", SourceType: "risk_event", SummaryText: "wrong org"},
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
		if arg.SpaceID != "space-1" || !arg.Column2.Valid || !arg.Column2.Time.Equal(inputWindowStart) || !arg.Column3.Valid || !arg.Column3.Time.Equal(inputWindowEnd) || arg.Limit <= 0 {
			t.Fatalf("unbounded timeline query = %+v", arg)
		}
	}
}

type staticSourceResolver struct{ inputs []ResolvedInput }

func (s staticSourceResolver) ResolveSource(context.Context, ResolveRequest, *Masker) ([]ResolvedInput, Coverage, []MissingInput, error) {
	return s.inputs, Coverage{}, nil, nil
}

func TestInputResolverSupportsPluggableSourcesAndBoundsOutput(t *testing.T) {
	const custom sdkdream.Source = "custom_sanitized"
	r := NewInputResolver(&fakeInputStore{})
	r.Register(custom, staticSourceResolver{inputs: []ResolvedInput{
		{SourceType: "spoofed", SourceID: "z", SanitizedText: "z private-9", Visibility: []string{"company"}},
		{SourceType: custom, SourceID: "a", SanitizedText: "a", Visibility: []string{"private"}},
		{SourceType: custom, SourceID: "a", SanitizedText: "duplicate", Visibility: []string{"private"}},
	}})
	inputs, coverage, _, err := r.Resolve(context.Background(), ResolveRequest{
		Sources: []sdkdream.Source{custom}, MaxInputs: 1, Visibility: []string{"company"}, MaskingRules: []string{`private-\d+`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].SourceType != custom || inputs[0].SourceID != "z" || strings.Contains(inputs[0].SanitizedText, "private-9") || coverage.InputCount != 1 {
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
