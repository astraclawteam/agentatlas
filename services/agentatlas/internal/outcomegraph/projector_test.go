package outcomegraph_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	og "github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// --- fixtures --------------------------------------------------------------

func ts() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) }

// outcomeFixture builds a versioned Outcome binding a goal, workcase,
// observation and optionally a contributor/evidence/blocker.
func outcomeFixture(tenant, key string, rev uint64, goalKey, caseID, contributor, evidence, blocker string) outcomemodel.Outcome {
	o := outcomemodel.Outcome{
		Tenant: tenant, OutcomeKey: key, Revision: rev,
		Claim: outcomemodel.OutcomeClaim{
			Goal:   outcomemodel.GoalRef{Tenant: tenant, GoalKey: goalKey, GoalVersion: 1},
			Status: outcomemodel.OutcomeSatisfied, RuleVersion: "rule-1",
			Observations: []outcomemodel.ObservationRef{{Handle: "obs-" + key, ObservationHash: "sha256:o", Authority: "system_of_record", SignatureKeyID: "k1", ObservedAt: ts()}},
		},
		WorkCaseID: caseID, WorkCaseRevision: 1, WorkPlanRevision: 1, OperatingMapVersion: 1, OrgVersion: 1,
		DecidedAt: ts(),
	}
	if contributor != "" {
		o.Contributions = []outcomemodel.ContributionRef{{ContributorID: contributor, Kind: "method", Weight: 0.5}}
	}
	if evidence != "" {
		o.Claim.Evidence = []outcomemodel.EvidenceRef{{Handle: evidence, ContentHash: "sha256:e", Authority: "system_of_record"}}
	}
	if blocker != "" {
		o.Claim.Blockers = []outcomemodel.BlockerRef{{Handle: blocker, Kind: "external", Authority: "system_of_record"}}
	}
	return o
}

// outcomeEvent wraps MapOutcome(o) into an upsert projection event.
func outcomeEvent(o outcomemodel.Outcome) og.ProjectionEvent {
	return og.ProjectionEvent{
		Tenant: o.Tenant, Source: og.SourceOutcome, Kind: og.KindUpsert,
		SubjectLabel: og.LabelOutcome, SubjectID: o.OutcomeKey, SubjectRevision: o.Revision,
		Delta: og.MapOutcome(o), RecordedAt: ts(),
	}.WithHash()
}

// workcaseEvent maps a WorkCase revision to its org-scoped node event.
func workcaseEvent(tenant, org, caseID string, rev uint64) og.ProjectionEvent {
	ev := og.MapWorkCase(sdkworkcase.WorkCase{ID: caseID, EnterpriseID: tenant, OrgScope: org, Status: sdkworkcase.Status("executing"), Revision: rev})
	ev.RecordedAt = ts()
	return ev
}

func mustAppend(t *testing.T, ob *og.MemOutbox, evs ...og.ProjectionEvent) {
	t.Helper()
	for _, e := range evs {
		if _, err := ob.Append(e); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
}

// --- basic projection ------------------------------------------------------

func TestProjectorBasicCatchUp(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	const tenant = "ent-A"

	mustAppend(t, ob,
		outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")),
		workcaseEvent(tenant, "org-1", "case-1", 1),
	)
	wm, err := p.ProjectTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if wm != 2 {
		t.Fatalf("watermark = %d, want 2", wm)
	}
	res, err := og.NewQueryService(g).TraceOutcomeBasis(ctx, tenant, "", "oc-1", 1, og.Budget{})
	if err != nil {
		t.Fatalf("trace basis: %v", err)
	}
	if len(res.Nodes) == 0 {
		t.Fatal("expected basis nodes for the projected outcome")
	}
	if res.Watermark != 2 {
		t.Fatalf("response watermark = %d, want 2", res.Watermark)
	}
	// The goal, observation, evidence and contributor must all be reachable.
	labels := map[og.NodeLabel]bool{}
	for _, n := range res.Nodes {
		labels[n.Label] = true
	}
	for _, want := range []og.NodeLabel{og.LabelGoal, og.LabelObservation, og.LabelEvidence, og.LabelContributor} {
		if !labels[want] {
			t.Errorf("basis missing %s node", want)
		}
	}
}

// --- idempotency: duplicate / redelivered / out-of-order -------------------

func TestProjectorDuplicateAndRedeliveryIdempotent(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	const tenant = "ent-A"
	mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")))

	if _, err := p.ProjectTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	d1 := g.Digest(tenant)
	// Re-run (redelivery) many times: watermark already at head, or replayed —
	// the graph must not change.
	for i := 0; i < 3; i++ {
		if _, err := p.ProjectTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	// Also apply the exact same batch directly again (duplicate delivery).
	evs, _ := ob.ReadAll(ctx, tenant)
	if err := g.ApplyBatch(ctx, tenant, evs); err != nil {
		t.Fatal(err)
	}
	if g.Digest(tenant) != d1 {
		t.Fatal("duplicate/redelivered application changed the graph (not idempotent)")
	}
}

func TestProjectorOutOfOrderConverges(t *testing.T) {
	ctx := context.Background()
	const tenant = "ent-A"
	evs := []og.ProjectionEvent{
		func() og.ProjectionEvent {
			e := outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", ""))
			e.Sequence = 1
			return e
		}(),
		func() og.ProjectionEvent { e := workcaseEvent(tenant, "org-1", "case-1", 1); e.Sequence = 2; return e }(),
		func() og.ProjectionEvent {
			e := outcomeEvent(outcomeFixture(tenant, "oc-2", 1, "goal.x", "case-2", "agent:planner", "ev-2", ""))
			e.Sequence = 3
			return e
		}(),
	}
	inOrder := og.NewMemoryGraph()
	if err := inOrder.ApplyBatch(ctx, tenant, evs); err != nil {
		t.Fatal(err)
	}
	shuffled := []og.ProjectionEvent{evs[2], evs[0], evs[1]}
	outOfOrder := og.NewMemoryGraph()
	if err := outOfOrder.ApplyBatch(ctx, tenant, shuffled); err != nil {
		t.Fatal(err)
	}
	if inOrder.Digest(tenant) != outOfOrder.Digest(tenant) {
		t.Fatal("out-of-order delivery converged to a different graph")
	}
}

// --- crash safety ----------------------------------------------------------

func TestProjectorCrashBeforeAGECommit(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	const tenant = "ent-A"
	mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")))

	// Crash: AGE is down when the projector tries to apply. The watermark must
	// NOT advance and the error must be explicit.
	g.SetUnavailable(true)
	if _, err := p.ProjectTenant(ctx, tenant); !errors.Is(err, og.ErrGraphUnavailable) {
		t.Fatalf("want ErrGraphUnavailable, got %v", err)
	}
	// Recovery: on the next run the projector catches up cleanly from the last
	// committed (zero) watermark.
	g.SetUnavailable(false)
	wm, err := p.ProjectTenant(ctx, tenant)
	if err != nil || wm != 1 {
		t.Fatalf("recovery project: wm=%d err=%v", wm, err)
	}
}

func TestProjectorCrashAfterAGECommitBeforeWatermark(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	const tenant = "ent-A"
	mustAppend(t, ob,
		outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")),
		workcaseEvent(tenant, "org-1", "case-1", 1),
	)

	// Simulate the crash window: the batch is applied to AGE (committed) but the
	// process dies BEFORE advancing the watermark, so the watermark is still 0.
	evs, _ := ob.ReadAll(ctx, tenant)
	if err := g.ApplyBatch(ctx, tenant, evs); err != nil {
		t.Fatal(err)
	}
	afterCommit := g.Digest(tenant)
	if wm, _ := g.Watermark(ctx, tenant); wm != 0 {
		t.Fatalf("precondition: watermark should still be 0, got %d", wm)
	}
	// On restart the projector re-reads from watermark 0 and replays the same
	// events. The deterministic merge keys make this a no-op: NO duplicate edges.
	wm, err := p.ProjectTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if wm != 2 {
		t.Fatalf("watermark after recovery = %d, want 2", wm)
	}
	if g.Digest(tenant) != afterCommit {
		t.Fatal("replay after a post-commit crash produced duplicate edges (not idempotent)")
	}
}

func TestProjectorStaleWatermarkAndRegression(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	const tenant = "ent-A"
	if err := g.AdvanceWatermark(ctx, tenant, 5); err != nil {
		t.Fatal(err)
	}
	if err := g.AdvanceWatermark(ctx, tenant, 3); !errors.Is(err, og.ErrWatermarkRegression) {
		t.Fatalf("want ErrWatermarkRegression, got %v", err)
	}
	if err := g.AdvanceWatermark(ctx, tenant, 5); err != nil {
		t.Fatalf("idempotent re-set should succeed: %v", err)
	}
	if err := g.AdvanceWatermark(ctx, tenant, 9); err != nil {
		t.Fatal(err)
	}
}

// --- rebuild equality ------------------------------------------------------

func TestRebuildEqualsIncremental(t *testing.T) {
	ctx := context.Background()
	const tenant = "ent-A"
	var events []og.ProjectionEvent
	events = append(events,
		outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")),
		workcaseEvent(tenant, "org-1", "case-1", 1),
		outcomeEvent(outcomeFixture(tenant, "oc-2", 1, "goal.x", "case-2", "agent:tuner", "ev-2", "blk-1")),
		workcaseEvent(tenant, "org-2", "case-2", 1),
	)
	for i := range events {
		events[i].Sequence = uint64(i + 1)
	}
	// Incremental: apply one event per batch.
	incremental := og.NewMemoryGraph()
	for _, e := range events {
		if err := incremental.ApplyBatch(ctx, tenant, []og.ProjectionEvent{e}); err != nil {
			t.Fatal(err)
		}
	}
	// Rebuild: drop + replay the whole outbox.
	rebuilt := og.NewMemoryGraph()
	if err := rebuilt.Rebuild(ctx, tenant, events); err != nil {
		t.Fatal(err)
	}
	if incremental.Digest(tenant) != rebuilt.Digest(tenant) {
		t.Fatal("rebuilt graph digest differs from the incrementally-projected graph")
	}
	// Rebuild is also idempotent.
	if err := rebuilt.Rebuild(ctx, tenant, events); err != nil {
		t.Fatal(err)
	}
	if incremental.Digest(tenant) != rebuilt.Digest(tenant) {
		t.Fatal("second rebuild diverged")
	}
	if wm, _ := rebuilt.Watermark(ctx, tenant); wm != uint64(len(events)) {
		t.Fatalf("rebuild watermark = %d, want %d", wm, len(events))
	}
}

// --- tombstone / revocation / supersession ---------------------------------

func TestProjectorEvidenceRevocationPropagates(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	const tenant = "ent-A"
	base := outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", ""))
	base.Sequence = 1
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{base}); err != nil {
		t.Fatal(err)
	}
	// Revoke the evidence node via a tombstone (append-only correction).
	tomb := og.ProjectionEvent{
		Tenant: tenant, Sequence: 2, Source: og.SourceOutcome, Kind: og.KindTombstone,
		SubjectLabel: og.LabelEvidence, SubjectID: "ev-1", SubjectRevision: 1,
		Delta: og.GraphDelta{Mark: &og.NodeRef{Label: og.LabelEvidence, BusinessID: "ev-1", Revision: 1}}, RecordedAt: ts(),
	}.WithHash()
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{tomb}); err != nil {
		t.Fatal(err)
	}
	// Replaying the tombstone stays idempotent.
	d := g.Digest(tenant)
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{tomb}); err != nil {
		t.Fatal(err)
	}
	if g.Digest(tenant) != d {
		t.Fatal("tombstone replay changed the graph")
	}
	res, err := og.NewQueryService(g).FindAffectedConclusions(ctx, tenant, "", "ev-1", og.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	basis, _ := og.NewQueryService(g).TraceOutcomeBasis(ctx, tenant, "", "oc-1", 1, og.Budget{})
	var sawRevoked bool
	for _, n := range basis.Nodes {
		if n.Label == og.LabelEvidence && n.BusinessID == "ev-1" {
			sawRevoked = n.Revoked
		}
	}
	if !sawRevoked {
		t.Fatal("revoked evidence not flagged revoked in the graph")
	}
}

func TestProjectorOutcomeSupersessionPropagates(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	const tenant = "ent-A"
	base := outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", ""))
	base.Sequence = 1
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{base}); err != nil {
		t.Fatal(err)
	}
	reeval := og.ProjectionEvent{
		Tenant: tenant, Sequence: 2, Source: og.SourceOutcome, Kind: og.KindReevaluation,
		SubjectLabel: og.LabelOutcome, SubjectID: "oc-1", SubjectRevision: 1,
		Delta: og.GraphDelta{Mark: &og.NodeRef{Label: og.LabelOutcome, BusinessID: "oc-1", Revision: 1}}, RecordedAt: ts(),
	}.WithHash()
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{reeval}); err != nil {
		t.Fatal(err)
	}
	// A later upsert of the SAME node must not clear the superseded flag.
	if err := g.ApplyBatch(ctx, tenant, []og.ProjectionEvent{base}); err != nil {
		t.Fatal(err)
	}
	res, _ := og.NewQueryService(g).ComparePlanOutcomes(ctx, tenant, "", "goal.x", 1, og.Budget{})
	var superseded bool
	for _, n := range res.Nodes {
		if n.Label == og.LabelOutcome && n.BusinessID == "oc-1" {
			superseded = n.Superseded
		}
	}
	if !superseded {
		t.Fatal("superseded outcome not flagged in the graph")
	}
}

// --- AGE outage + recovery -------------------------------------------------

func TestProjectorAGEOutageDegradesThenRecovers(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	qs := og.NewQueryService(g)
	const tenant = "ent-A"
	mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")))
	if _, err := p.ProjectTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	// AGE goes down: graph-dependent queries degrade EXPLICITLY (never a silent
	// wrong answer), and further projection does not advance the watermark.
	g.SetUnavailable(true)
	if _, err := qs.TraceOutcomeBasis(ctx, tenant, "", "oc-1", 1, og.Budget{}); !errors.Is(err, og.ErrGraphUnavailable) {
		t.Fatalf("query during outage: want ErrGraphUnavailable, got %v", err)
	}
	mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, "oc-2", 1, "goal.x", "case-2", "agent:planner", "ev-2", "")))
	if _, err := p.ProjectTenant(ctx, tenant); !errors.Is(err, og.ErrGraphUnavailable) {
		t.Fatalf("project during outage: want ErrGraphUnavailable, got %v", err)
	}

	// Recovery: the projector catches up from the last committed watermark with
	// no duplicate edges.
	g.SetUnavailable(false)
	wm, err := p.ProjectTenant(ctx, tenant)
	if err != nil || wm != 2 {
		t.Fatalf("recovery: wm=%d err=%v", wm, err)
	}
	res, err := qs.TraceOutcomeBasis(ctx, tenant, "", "oc-2", 1, og.Budget{})
	if err != nil || len(res.Nodes) == 0 {
		t.Fatalf("post-recovery query: nodes=%d err=%v", len(res.Nodes), err)
	}
}

// --- concurrent per-tenant projection is serialized (race-safe, no dup) -----

func TestProjectorConcurrentSerialization(t *testing.T) {
	ctx := context.Background()
	const tenant = "ent-A"
	// Build the outbox once.
	ob := og.NewMemOutbox()
	for i := 0; i < 6; i++ {
		mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, fmt.Sprintf("oc-%d", i), 1, "goal.x", fmt.Sprintf("case-%d", i), "agent:planner", fmt.Sprintf("ev-%d", i), "")))
	}
	// Sequential baseline digest.
	seq := og.NewMemoryGraph()
	if _, err := og.NewProjector(seq, ob).ProjectTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	want := seq.Digest(tenant)

	// Concurrent: many projectors over ONE shared graph + outbox. ProjectBatch's
	// per-tenant serialization must keep this race-safe (passes under -race) and
	// converge to the same graph.
	g := og.NewMemoryGraph()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := og.NewProjector(g, ob)
			<-start
			_, _ = p.ProjectTenant(ctx, tenant)
		}()
	}
	close(start)
	wg.Wait()
	if got := g.Digest(tenant); got != want {
		t.Fatalf("concurrent projection diverged from sequential:\n got=%s\nwant=%s", got, want)
	}
	if wm, _ := g.Watermark(ctx, tenant); wm != 6 {
		t.Fatalf("watermark after concurrent projection = %d, want 6", wm)
	}
}

// --- BLOCKER mutation-proof: serialization prevents MERGE match-miss dup ----

// racyGraph models a graph provider whose "MERGE" on the nid PROPERTY has NO
// unique constraint (exactly AGE's situation: only its internal id is unique).
// The MERGE is a NON-ATOMIC check-then-insert. When serialize is false there is
// no per-tenant lock, so two concurrent projectors both check-miss the same nid
// and both insert -> a DUPLICATE vertex. When serialize is true a single
// per-tenant lock (modeling the graph-database advisory lock AGEStore.lockTenant
// takes) covers the whole read-watermark -> apply -> advance unit, so no
// duplicates occur. This is the deterministic mutation-proof of the fix: flip
// serialize off and duplicates appear; on and they do not.
type racyGraph struct {
	serialize bool
	outer     sync.Mutex // the per-tenant projection lock (the thing under test)
	inner     sync.Mutex // protects the slice itself (no Go data race)
	nodes     []string   // appended nids; duplicates possible without serialization
	wm        uint64
	seam      *readWriteSeam // unserialized runs only; nil when serialize is true
}

// readWriteSeam forces the outcome of the race instead of hoping for it. It
// holds a worker inside the non-atomic MERGE window -- after it has read "this
// nid does not exist yet" and before it writes -- until it can tell whether a
// second worker got into that same window.
//
// It replaces a runtime.Gosched() at this spot. Gosched only HINTS that the
// scheduler should switch, so under load (a full-repo `go test ./...`) the
// runtime could serialize the workers by accident: no duplicate appeared and
// the control assertion failed for a reason unrelated to the code under test.
//
// Crucially the seam runs in BOTH modes. An earlier version engaged it only in
// the unserialized run, which left the serialized assertion vacuous -- deleting
// the per-tenant lock entirely still passed 10/10, because without forced
// interleaving the workers simply never collided.
type readWriteSeam struct {
	mu       sync.Mutex
	total    int // workers in this run
	inWindow int // inside the read->write window, not yet released
	blocked  int // parked on the serialization lock, so unable to reach the window
	finished int // done with ProjectBatch
	opened   bool
	open     chan struct{}
}

func newReadWriteSeam(total int) *readWriteSeam {
	return &readWriteSeam{total: total, open: make(chan struct{})}
}

// releaseLocked opens the seam once the outcome is decided either way:
//
//	inWindow >= 2                          two workers collided -- the hazard is real
//	inWindow + blocked + finished >= total no one else can possibly arrive
//
// The second clause is what makes this deadlock-free under serialization: the
// lock holder sits in the window while every other worker is parked on the
// lock, so the run is fully accounted for and the holder is released alone --
// which is exactly the property "serialized" is supposed to have.
//
// It also cannot fire early in the unserialized run: nothing can reach
// `finished` before the first append, and the first append is downstream of
// this seam.
func (s *readWriteSeam) releaseLocked() {
	if s.opened {
		return
	}
	if s.inWindow >= 2 || s.inWindow+s.blocked+s.finished >= s.total {
		s.opened = true
		close(s.open)
	}
}

func (s *readWriteSeam) hold() {
	s.mu.Lock()
	if s.opened {
		s.mu.Unlock()
		return
	}
	s.inWindow++
	s.releaseLocked()
	opened := s.opened
	s.mu.Unlock()
	if !opened {
		<-s.open
	}
}

// blocking/unblocked bracket the wait on the serialization lock so the seam can
// tell "no one else is coming" from "no one else has got here yet".
func (s *readWriteSeam) blocking() {
	s.mu.Lock()
	s.blocked++
	s.releaseLocked()
	s.mu.Unlock()
}

func (s *readWriteSeam) unblocked() {
	s.mu.Lock()
	s.blocked--
	s.mu.Unlock()
}

func (s *readWriteSeam) finish() {
	s.mu.Lock()
	s.finished++
	s.releaseLocked()
	s.mu.Unlock()
}

func (r *racyGraph) ProjectBatch(_ context.Context, _ string, events []og.ProjectionEvent) (uint64, error) {
	defer r.seam.finish()
	if r.serialize {
		r.seam.blocking()
		r.outer.Lock()
		r.seam.unblocked()
		defer r.outer.Unlock()
	}
	r.inner.Lock()
	wm := r.wm
	r.inner.Unlock()
	last := wm
	for _, e := range events {
		if e.Sequence <= wm {
			continue
		}
		for _, n := range e.Delta.Nodes {
			id := n.NID()
			r.inner.Lock()
			exists := false
			for _, x := range r.nodes {
				if x == id {
					exists = true
					break
				}
			}
			r.inner.Unlock()
			if !exists {
				// Hold between the read and the write until the seam knows
				// whether a second worker made it into the same window. Runs in
				// both modes: under serialization it releases this worker alone
				// (everyone else is parked on outer), which is the property the
				// serialized assertion is actually claiming.
				r.seam.hold()
				r.inner.Lock()
				r.nodes = append(r.nodes, id)
				r.inner.Unlock()
			}
		}
		if e.Sequence > last {
			last = e.Sequence
		}
	}
	r.inner.Lock()
	if last > r.wm {
		r.wm = last
	}
	r.inner.Unlock()
	return last, nil
}

func (r *racyGraph) maxDup() int {
	r.inner.Lock()
	defer r.inner.Unlock()
	counts := map[string]int{}
	best := 0
	for _, id := range r.nodes {
		counts[id]++
		if counts[id] > best {
			best = counts[id]
		}
	}
	return best
}

func (r *racyGraph) Watermark(_ context.Context, _ string) (uint64, error) {
	r.inner.Lock()
	defer r.inner.Unlock()
	return r.wm, nil
}
func (r *racyGraph) ApplyBatch(context.Context, string, []og.ProjectionEvent) error { return nil }
func (r *racyGraph) AdvanceWatermark(context.Context, string, uint64) error         { return nil }
func (r *racyGraph) Rebuild(context.Context, string, []og.ProjectionEvent) error    { return nil }
func (r *racyGraph) GetLineage(context.Context, og.LineageQuery) (og.QueryResult, error) {
	return og.QueryResult{}, nil
}
func (r *racyGraph) FindRelatedCases(context.Context, og.RelatedCasesQuery) (og.QueryResult, error) {
	return og.QueryResult{}, nil
}
func (r *racyGraph) TraceImpact(context.Context, og.ImpactQuery) (og.QueryResult, error) {
	return og.QueryResult{}, nil
}
func (r *racyGraph) QueryMethods(context.Context, og.MethodsQuery) (og.QueryResult, error) {
	return og.QueryResult{}, nil
}

func TestPerTenantSerializationPreventsDuplicates(t *testing.T) {
	ctx := context.Background()
	const tenant = "ent-A"
	run := func(serialize bool) int {
		ob := og.NewMemOutbox()
		for i := 0; i < 6; i++ {
			mustAppend(t, ob, outcomeEvent(outcomeFixture(tenant, fmt.Sprintf("oc-%d", i), 1, "goal.x", fmt.Sprintf("case-%d", i), "agent:planner", "", "")))
		}
		const workers = 8
		g := &racyGraph{serialize: serialize, seam: newReadWriteSeam(workers)}
		var _ og.OutcomeGraphStore = g // must satisfy the provider interface
		start := make(chan struct{})
		var wg sync.WaitGroup
		for k := 0; k < workers; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := og.NewProjector(g, ob)
				<-start
				_, _ = p.ProjectTenant(ctx, tenant)
			}()
		}
		close(start)
		wg.Wait()
		return g.maxDup()
	}
	// Mutation control: WITHOUT per-tenant serialization the match-miss model
	// duplicates vertices (proves the hazard is real and the test can detect it).
	if dup := run(false); dup <= 1 {
		t.Fatalf("control: without serialization the racy MERGE model should duplicate, got max/nid=%d", dup)
	}
	// WITH serialization (the fix): exactly one vertex per nid.
	if dup := run(true); dup != 1 {
		t.Fatalf("with per-tenant serialization there must be NO duplicates, got max/nid=%d", dup)
	}
}

// --- forbidden pruning -----------------------------------------------------

func TestProjectorNeverPrunesOutbox(t *testing.T) {
	ctx := context.Background()
	g := og.NewMemoryGraph()
	ob := og.NewMemOutbox()
	p := og.NewProjector(g, ob)
	const tenant = "ent-A"
	mustAppend(t, ob,
		outcomeEvent(outcomeFixture(tenant, "oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1", "")),
		outcomeEvent(outcomeFixture(tenant, "oc-2", 1, "goal.x", "case-2", "agent:planner", "ev-2", "")),
	)
	before, _ := ob.ReadAll(ctx, tenant)
	if _, err := p.ProjectTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	after, _ := ob.ReadAll(ctx, tenant)
	if len(after) != len(before) || len(after) != 2 {
		t.Fatalf("projector must not prune the outbox: before=%d after=%d", len(before), len(after))
	}
	// The OutboxReader interface exposes no delete/prune method — pruning is
	// structurally impossible from the projector.
}
