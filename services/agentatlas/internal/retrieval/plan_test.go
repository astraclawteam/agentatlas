package retrieval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

func TestIndexNamePerEnterprise(t *testing.T) {
	if got := IndexName("ent_Alpha 01"); got != "atlas-ent_alpha-01-knowledge" {
		t.Fatalf("index name = %q", got)
	}
	if IndexName("entA") == IndexName("entB") {
		t.Fatal("enterprises must map to distinct indexes")
	}
}

func TestBuildKeywordQueryShape(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	q := Query{
		EnterpriseID: "ent_1", Text: "异常工单",
		SpaceIDs: []string{"spc_1"}, SourceTypes: []string{"work_brief"},
		MaxRiskLevel: "low", From: &from, TopK: 5,
	}
	raw, _ := json.Marshal(BuildKeywordQuery(q))
	s := string(raw)
	for _, must := range []string{
		`"multi_match"`, `"summary_text^2"`, `"enterprise_id":"ent_1"`,
		`"space_id":["spc_1"]`, `"source_type":["work_brief"]`, `"range"`, `"size":5`,
	} {
		if !contains(s, must) {
			t.Fatalf("keyword query missing %s: %s", must, s)
		}
	}
	// low max risk must not admit medium/high
	if contains(s, `"medium"`) || contains(s, `"high"`) {
		t.Fatalf("risk expansion leaked higher levels: %s", s)
	}
}

func TestBuildVectorQueryShape(t *testing.T) {
	raw, _ := json.Marshal(BuildVectorQuery(Query{EnterpriseID: "ent_1", Text: "q", TopK: 3}, []float32{0.1, 0.2}))
	s := string(raw)
	if !contains(s, `"knn"`) || !contains(s, `"k":3`) || !contains(s, `"enterprise_id":"ent_1"`) {
		t.Fatalf("vector query shape: %s", s)
	}
}

func TestRRFMergeDedupesAndRanks(t *testing.T) {
	keyword := []Hit{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	vector := []Hit{{ID: "b"}, {ID: "d"}}
	fused := RRFMerge(keyword, vector, 10)
	if len(fused) != 4 {
		t.Fatalf("fused = %d", len(fused))
	}
	if fused[0].ID != "b" {
		t.Fatalf("doc in both rankings must fuse to top, got %s", fused[0].ID)
	}
	seen := map[string]bool{}
	for _, h := range fused {
		if seen[h.ID] {
			t.Fatalf("duplicate %s", h.ID)
		}
		seen[h.ID] = true
	}
	again := RRFMerge(keyword, vector, 10)
	for i := range fused {
		if fused[i].ID != again[i].ID {
			t.Fatal("RRF merge must be deterministic")
		}
	}
}

type fakePlanStore struct {
	plans   []db.CreateRetrievalPlanParams
	steps   []db.InsertRetrievalPlanStepParams
	results []db.InsertRetrievalResultParams
}

func (f *fakePlanStore) CreateRetrievalPlan(_ context.Context, arg db.CreateRetrievalPlanParams) (db.RetrievalPlan, error) {
	f.plans = append(f.plans, arg)
	return db.RetrievalPlan{ID: arg.ID, EnterpriseID: arg.EnterpriseID}, nil
}
func (f *fakePlanStore) InsertRetrievalPlanStep(_ context.Context, arg db.InsertRetrievalPlanStepParams) error {
	f.steps = append(f.steps, arg)
	return nil
}
func (f *fakePlanStore) InsertRetrievalResult(_ context.Context, arg db.InsertRetrievalResultParams) error {
	f.results = append(f.results, arg)
	return nil
}

type fakeSearch struct {
	docs map[string]IndexDocument // id -> doc (single index fake)
}

func (f *fakeSearch) EnsureIndex(context.Context, string) error { return nil }
func (f *fakeSearch) Index(_ context.Context, _ string, id string, doc any) error {
	raw, _ := json.Marshal(doc)
	var d IndexDocument
	_ = json.Unmarshal(raw, &d)
	f.docs[id] = d
	return nil
}
func (f *fakeSearch) Search(_ context.Context, _ string, _ any) (SearchResult, error) {
	var hits []Hit
	for id, d := range f.docs {
		raw, _ := json.Marshal(d)
		hits = append(hits, Hit{ID: id, Score: 1, Source: raw})
	}
	return SearchResult{Total: len(hits), Hits: hits}, nil
}

func TestCreatePlanPersistsPipelineSteps(t *testing.T) {
	store := &fakePlanStore{}
	svc := NewService(store, &fakeSearch{docs: map[string]IndexDocument{}}, nil, nil)
	planID, err := svc.CreatePlan(context.Background(), Query{EnterpriseID: "ent_1", Text: "我的工作内容"})
	if err != nil || planID == "" {
		t.Fatalf("plan: %v", err)
	}
	if len(store.steps) != 2 { // keyword + filter (no embedder/reranker configured)
		t.Fatalf("steps = %d: %+v", len(store.steps), store.steps)
	}
	if store.steps[0].Kind != "keyword" || store.steps[1].Kind != "filter" {
		t.Fatalf("pipeline order: %+v", store.steps)
	}
	if store.plans[0].SanitizedQuery != "我的工作内容" {
		t.Fatalf("sanitized query: %+v", store.plans[0])
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
