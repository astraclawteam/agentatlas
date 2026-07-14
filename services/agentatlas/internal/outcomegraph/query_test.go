package outcomegraph_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	og "github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// seed builds a small two-tenant, two-org graph for the query tests.
func seed(t *testing.T) *og.MemoryGraph {
	t.Helper()
	ctx := context.Background()
	g := og.NewMemoryGraph()
	// Tenant A, org X and org Y, both concerning goal.shared.
	evs := []og.ProjectionEvent{
		outcomeEvent(outcomeFixture("ent-A", "oc-x", 1, "goal.shared", "case-x", "agent:planner", "ev-x", "")),
		workcaseEvent("ent-A", "org-x", "case-x", 1),
		outcomeEvent(outcomeFixture("ent-A", "oc-y", 1, "goal.shared", "case-y", "agent:tuner", "ev-y", "")),
		workcaseEvent("ent-A", "org-y", "case-y", 1),
	}
	if err := g.ApplyBatch(ctx, "ent-A", evs); err != nil {
		t.Fatal(err)
	}
	// Tenant B: a DIFFERENT tenant with the SAME goal key and case id, to prove
	// cross-tenant isolation.
	bEvs := []og.ProjectionEvent{
		outcomeEvent(outcomeFixture("ent-B", "oc-x", 1, "goal.shared", "case-x", "agent:planner", "ev-x", "")),
		workcaseEvent("ent-B", "org-x", "case-x", 1),
	}
	if err := g.ApplyBatch(ctx, "ent-B", bEvs); err != nil {
		t.Fatal(err)
	}
	return g
}

// --- budget ----------------------------------------------------------------

func TestQueryBudgetExcessiveRefused(t *testing.T) {
	ctx := context.Background()
	qs := og.NewQueryService(og.NewMemoryGraph())
	cases := map[string]og.Budget{
		"depth": {MaxDepth: og.CeilingMaxDepth + 1},
		"rows":  {MaxRows: og.CeilingMaxRows + 1},
		"time":  {Timeout: og.CeilingTimeout + time.Second},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, b)
			if !errors.Is(err, og.ErrBudgetExceeded) {
				t.Fatalf("want ErrBudgetExceeded, got %v", err)
			}
		})
	}
}

func TestQueryCancelledContext(t *testing.T) {
	g := seed(t)
	qs := og.NewQueryService(g)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, og.Budget{}); err == nil {
		t.Fatal("expected a cancelled-context query to fail, got nil")
	}
}

func TestQueryRowBudgetTruncates(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	qs := og.NewQueryService(g)
	// A tiny row budget must bound the result, never run unbounded.
	res, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, og.Budget{MaxRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) > 1 {
		t.Fatalf("row budget not enforced: %d nodes", len(res.Nodes))
	}
	if !res.Truncated {
		t.Fatal("truncation not reported")
	}
}

// --- arbitrary Cypher / typed-only surface ---------------------------------

func TestQueryArbitraryCypherRejected(t *testing.T) {
	ctx := context.Background()
	qs := og.NewQueryService(seed(t))
	// An unknown/forged operation is rejected outright — there is no raw-Cypher
	// entry point at all.
	_, err := qs.Run(ctx, og.Request{Operation: og.Operation("MATCH (n) DETACH DELETE n"), Tenant: "ent-A", Anchor: og.NodeRef{Label: og.LabelOutcome, BusinessID: "oc-x", Revision: 1}})
	if !errors.Is(err, og.ErrArbitraryCypher) {
		t.Fatalf("want ErrArbitraryCypher, got %v", err)
	}
	if _, err := qs.Run(ctx, og.Request{Operation: "", Tenant: "ent-A"}); !errors.Is(err, og.ErrArbitraryCypher) {
		t.Fatalf("empty op: want ErrArbitraryCypher, got %v", err)
	}
}

func TestQueryParamValidation(t *testing.T) {
	ctx := context.Background()
	qs := og.NewQueryService(seed(t))
	// Missing tenant.
	if _, err := qs.TraceOutcomeBasis(ctx, "", "", "oc-x", 1, og.Budget{}); !errors.Is(err, og.ErrInvalidQuery) {
		t.Fatalf("missing tenant: want ErrInvalidQuery, got %v", err)
	}
	// Wrong anchor label for the operation.
	if _, err := qs.Run(ctx, og.Request{Operation: og.OpTraceOutcomeBasis, Tenant: "ent-A", Anchor: og.NodeRef{Label: og.LabelWorkCase, BusinessID: "x", Revision: 1}}); !errors.Is(err, og.ErrInvalidQuery) {
		t.Fatalf("wrong anchor label: want ErrInvalidQuery, got %v", err)
	}
	// Unversioned anchor.
	if _, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "", 0, og.Budget{}); !errors.Is(err, og.ErrInvalidQuery) {
		t.Fatalf("unversioned anchor: want ErrInvalidQuery, got %v", err)
	}
}

// --- injection safety ------------------------------------------------------

func TestQueryInjectionParamIsInertLiteral(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	qs := og.NewQueryService(g)
	before := g.Digest("ent-A")
	// A Cypher-breakout attempt in the id parameter must be treated as an inert
	// literal: it matches no node, returns nothing, and mutates nothing.
	inj := `oc-x") DETACH DELETE (n) MERGE (z:Outcome {nid:"pwned"}) RETURN n //`
	res, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", inj, 1, og.Budget{})
	if err != nil {
		t.Fatalf("injection query should run safely, got %v", err)
	}
	if len(res.Nodes) != 0 {
		t.Fatalf("injection id matched %d nodes, want 0", len(res.Nodes))
	}
	if g.Digest("ent-A") != before {
		t.Fatal("injection attempt mutated the graph")
	}
}

// --- tenant / org isolation ------------------------------------------------

func TestQueryCrossTenantIsolation(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	qs := og.NewQueryService(g)
	// Tenant A's basis must never surface tenant B's nodes even though B shares
	// the goal key and case id.
	res, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) == 0 {
		t.Fatal("expected tenant A basis nodes")
	}
	// Tenant B queried from A's outcome id returns nothing (isolation), not B's data.
	resB, err := qs.TraceOutcomeBasis(ctx, "ent-B", "", "oc-y", 1, og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resB.Nodes) != 0 {
		t.Fatal("tenant B saw an outcome that only exists in tenant A (cross-tenant leak)")
	}
}

func TestQueryCrossOrgIsolation(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	qs := og.NewQueryService(g)
	// find_similar_workcases from case-x (org-x): tenant-scoped sees the org-y
	// sibling; org-x-scoped must NOT (org isolation).
	all, err := qs.FindSimilarWorkcases(ctx, "ent-A", "", "case-x", 1, og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	var sawYTenantScoped bool
	for _, n := range all.Nodes {
		if n.BusinessID == "case-y" {
			sawYTenantScoped = true
		}
	}
	if !sawYTenantScoped {
		t.Fatal("tenant-scoped find_similar_workcases should reach the sibling case-y")
	}
	scoped, err := qs.FindSimilarWorkcases(ctx, "ent-A", "org-x", "case-x", 1, og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range scoped.Nodes {
		if n.BusinessID == "case-y" {
			t.Fatal("org-x query returned an org-y case (cross-org leak)")
		}
	}
}

// --- classified payload canary ---------------------------------------------

func TestClassifiedPayloadCanaryRejected(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	const tenant = "ent-A"
	// A sentinel of raw/sensitive content (a connector DSN with a credential) in
	// a node summary must be REJECTED — it can never enter the graph.
	canary := "jdbc:postgresql://svc:S3cr3t@erp.internal:5432/prod"
	badEvent := og.ProjectionEvent{
		Tenant: tenant, Source: og.SourceWorkCase, Kind: og.KindUpsert,
		SubjectLabel: og.LabelWorkCase, SubjectID: "case-canary", SubjectRevision: 1,
		Delta: og.GraphDelta{Nodes: []og.GraphNode{{
			Tenant: tenant, Org: "org-x", Label: og.LabelWorkCase, BusinessID: "case-canary", Revision: 1, Summary: canary,
		}}}, RecordedAt: ts(),
	}
	if err := badEvent.Validate(); err == nil {
		t.Fatal("a connector-shaped canary summary must fail validation")
	}
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{badEvent}); err == nil {
		t.Fatal("ApplyBatch must reject a classified-payload canary")
	}
	// The canary never reached any node, so no query can echo it.
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{
		outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")),
	}); err != nil {
		t.Fatal(err)
	}
	res, _ := og.NewQueryService(g).TraceOutcomeBasis(ctx, tenant, "", "oc-1", 1, og.Budget{})
	for _, n := range res.Nodes {
		if strings.Contains(n.Summary, "S3cr3t") || strings.Contains(n.Hash, "S3cr3t") {
			t.Fatal("canary content surfaced in a query response")
		}
	}
}

// --- freshness envelope ----------------------------------------------------

func TestEveryResponseCarriesRevisionAndWatermark(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	if err := g.AdvanceWatermark(ctx, "ent-A", 4); err != nil {
		t.Fatal(err)
	}
	qs := og.NewQueryService(g)
	res, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Watermark != 4 {
		t.Fatalf("response watermark = %d, want 4", res.Watermark)
	}
	if res.SourceRevision != 1 {
		t.Fatalf("response source revision = %d, want 1", res.SourceRevision)
	}
}

// --- label whitelist (injection linchpin) negative tests -------------------

func TestUnknownLabelsRejected(t *testing.T) {
	ctx := context.Background()
	// A node with an unknown label (would be fmt.Sprintf'd into Cypher) is rejected.
	if err := (og.GraphNode{Tenant: "t", Label: og.NodeLabel("Bogus"), BusinessID: "x", Revision: 1}).Validate(); err == nil {
		t.Fatal("unknown NodeLabel must be rejected by GraphNode.Validate")
	}
	// An edge with an unknown edge label is rejected.
	badEdge := og.GraphEdge{Tenant: "t", Label: og.EdgeLabel("PWN"), From: og.NodeRef{Label: og.LabelOutcome, BusinessID: "a", Revision: 1}, To: og.NodeRef{Label: og.LabelGoal, BusinessID: "b", Revision: 1}}
	if err := badEdge.Validate(); err == nil {
		t.Fatal("unknown EdgeLabel must be rejected by GraphEdge.Validate")
	}
	// An edge with an unknown ENDPOINT label is rejected.
	badEnd := og.GraphEdge{Tenant: "t", Label: og.EdgeConcerns, From: og.NodeRef{Label: og.NodeLabel("Bogus"), BusinessID: "a", Revision: 1}, To: og.NodeRef{Label: og.LabelGoal, BusinessID: "b", Revision: 1}}
	if err := badEnd.Validate(); err == nil {
		t.Fatal("unknown edge-endpoint NodeLabel must be rejected")
	}
	// A standalone ref with an unknown label is rejected.
	if err := (og.NodeRef{Label: og.NodeLabel("Nope"), BusinessID: "a", Revision: 1}).Validate(); err == nil {
		t.Fatal("unknown NodeRef label must be rejected")
	}
	// ApplyBatch rejects an unknown-label node BEFORE it ever reaches Cypher.
	g := og.NewMemoryGraph()
	ev := og.ProjectionEvent{Tenant: "t", Source: og.SourceWorkCase, Kind: og.KindUpsert, SubjectLabel: og.LabelWorkCase, SubjectID: "x", SubjectRevision: 1,
		Delta: og.GraphDelta{Nodes: []og.GraphNode{{Tenant: "t", Label: og.NodeLabel("Bogus"), BusinessID: "x", Revision: 1}}}, RecordedAt: ts()}
	if err := g.ApplyBatch(ctx, "t", []og.ProjectionEvent{ev}); err == nil {
		t.Fatal("ApplyBatch must reject an unknown-label node before interpolation")
	}
	// The query dispatcher rejects an anchor whose label is not the operation's.
	if _, err := og.NewQueryService(g).Run(ctx, og.Request{Operation: og.OpTraceOutcomeBasis, Tenant: "t", Anchor: og.NodeRef{Label: og.NodeLabel("Bogus"), BusinessID: "x", Revision: 1}}); err == nil {
		t.Fatal("query with an unknown/mismatched anchor label must be rejected")
	}
}

// --- all 7 typed operations reachable --------------------------------------

func TestAllTypedOperations(t *testing.T) {
	ctx := context.Background()
	g := seed(t)
	qs := og.NewQueryService(g)
	b := og.Budget{}
	if _, err := qs.TraceOutcomeBasis(ctx, "ent-A", "", "oc-x", 1, b); err != nil {
		t.Errorf("trace_outcome_basis: %v", err)
	}
	if _, err := qs.FindSimilarWorkcases(ctx, "ent-A", "", "case-x", 1, b); err != nil {
		t.Errorf("find_similar_workcases: %v", err)
	}
	if _, err := qs.TraceBlockerImpact(ctx, "ent-A", "", "blk-1", b); err != nil {
		t.Errorf("trace_blocker_impact: %v", err)
	}
	methods, err := qs.FindEffectiveMethods(ctx, "ent-A", "", "goal.shared", 1, b)
	if err != nil {
		t.Errorf("find_effective_methods: %v", err)
	}
	if len(methods.Nodes) == 0 {
		t.Error("find_effective_methods returned no contributors for a goal with satisfied outcomes")
	}
	if _, err := qs.ExplainContribution(ctx, "ent-A", "", "agent:planner", b); err != nil {
		t.Errorf("explain_contribution: %v", err)
	}
	if _, err := qs.FindAffectedConclusions(ctx, "ent-A", "", "ev-x", b); err != nil {
		t.Errorf("find_affected_conclusions: %v", err)
	}
	if _, err := qs.ComparePlanOutcomes(ctx, "ent-A", "", "goal.shared", 1, b); err != nil {
		t.Errorf("compare_plan_outcomes: %v", err)
	}
	if len(og.Operations()) != 7 {
		t.Fatalf("expected 7 typed operations, got %d", len(og.Operations()))
	}
}
