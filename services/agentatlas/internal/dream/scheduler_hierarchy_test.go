package dream

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSchedulerHierarchyDueUsesEnterpriseTimezone(t *testing.T) {
	p := validPolicy()
	start, end, due, err := Due(p, time.Time{}, time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC))
	if err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	wantEnd := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if !end.Equal(wantEnd) || !start.Equal(time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("window=%s..%s want=%s..%s", start, end, wantEnd.Add(-24*time.Hour), wantEnd)
	}
}

func TestSchedulerHierarchyDuePreservesDSTLocalSchedule(t *testing.T) {
	p := validPolicy()
	p.Timezone = "America/New_York"
	p.Schedule = "0 2 * * *"
	last := time.Date(2026, 3, 7, 7, 0, 0, 0, time.UTC) // 02:00 EST
	start, end, due, err := Due(p, last, time.Date(2026, 3, 9, 6, 1, 0, 0, time.UTC))
	if err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	// March 8 has no 02:00 local firing; the next exact local firing is
	// March 9 02:00 EDT (06:00 UTC).
	if !start.Equal(last) || !end.Equal(time.Date(2026, 3, 9, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("DST window=%s..%s", start, end)
	}
}

func TestSchedulerHierarchyOrdersChildrenBeforeParents(t *testing.T) {
	candidates := []HierarchyCandidate{
		{PolicyID: "company", OrgUnitID: "company:acme", Depth: 0},
		{PolicyID: "team-b", OrgUnitID: "project_group:b", Depth: 2},
		{PolicyID: "dept", OrgUnitID: "department:rd", Depth: 1},
		{PolicyID: "team-a", OrgUnitID: "project_group:a", Depth: 2},
	}
	sortHierarchyCandidates(candidates)
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, candidate.PolicyID)
	}
	want := []string{"team-a", "team-b", "dept", "company"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want=%v", got, want)
	}
}

func TestSchedulerHierarchyReadinessRequiresEveryImmediateChild(t *testing.T) {
	expected := []string{"project_group:a", "project_group:b"}
	runs := []ChildRunState{{OrgUnitID: "project_group:a", Status: "succeeded", Attempt: 1}}
	ready, coverage, missing, err := childReadiness(expected, runs, nil, false, map[string]int32{"project_group:a": 3, "project_group:b": 3})
	if err != nil {
		t.Fatal(err)
	}
	if ready || coverage.ExpectedChildren != 2 || coverage.CompletedChildren != 1 || len(missing) != 0 {
		t.Fatalf("ready=%v coverage=%+v missing=%+v", ready, coverage, missing)
	}

	explicit := []MissingInput{{SourceType: sdkdream.SourceChildDreamSummary, SourceID: "project_group:b", Reason: sdkdream.MissingNotFound}}
	ready, coverage, missing, err = childReadiness(expected, runs, explicit, false, map[string]int32{"project_group:a": 3, "project_group:b": 3})
	if err != nil || !ready || coverage.CompletedChildren != 1 || !reflect.DeepEqual(missing, explicit) {
		t.Fatalf("ready=%v coverage=%+v missing=%+v err=%v", ready, coverage, missing, err)
	}
}

func TestSchedulerHierarchyPartialModeOnlyAcceptsTerminalFailure(t *testing.T) {
	expected := []string{"project_group:a"}
	for _, tc := range []struct {
		name         string
		allowPartial bool
		attempt      int32
		wantReady    bool
	}{
		{name: "partial disabled", allowPartial: false, attempt: 3},
		{name: "retry remains", allowPartial: true, attempt: 2},
		{name: "terminal partial", allowPartial: true, attempt: 3, wantReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready, coverage, missing, err := childReadiness(expected, []ChildRunState{{OrgUnitID: expected[0], Status: "failed", Attempt: tc.attempt}}, nil, tc.allowPartial, map[string]int32{expected[0]: 3})
			if err != nil || ready != tc.wantReady {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			if tc.wantReady && (coverage.CompletedChildren != 0 || len(missing) != 1 || missing[0].Reason != sdkdream.MissingFailed) {
				t.Fatalf("coverage=%+v missing=%+v", coverage, missing)
			}
		})
	}
}

func TestSchedulerHierarchyDeterministicIDsIncludeVersionAndSequence(t *testing.T) {
	end := time.Date(2026, 7, 10, 14, 0, 0, 123, time.UTC)
	base := runIDFor("policy", 7, end, 0)
	if base != runIDFor("policy", 7, end, 0) {
		t.Fatal("same window identity changed")
	}
	for _, other := range stringSet(
		runIDFor("policy", 8, end, 0),
		runIDFor("policy", 7, end, 1),
		runIDFor("policy", 7, end.Add(time.Nanosecond), 0),
	) {
		if other == base {
			t.Fatalf("identity collision with %q", other)
		}
	}
}

func TestSchedulerHierarchyRetryCap(t *testing.T) {
	for _, tc := range []struct {
		status   string
		attempt  int32
		max      int32
		want     bool
		wantNext int32
	}{
		{status: "failed", attempt: 1, max: 3, want: true, wantNext: 2},
		{status: "failed", attempt: 3, max: 3},
		{status: "succeeded", attempt: 1, max: 3},
	} {
		next, ok := retryAttempt(tc.status, tc.attempt, tc.max)
		if ok != tc.want || next != tc.wantNext {
			t.Fatalf("status=%s attempt=%d max=%d => (%d,%v)", tc.status, tc.attempt, tc.max, next, ok)
		}
	}
}

func TestSchedulerHierarchyRerunAndBackfillValidation(t *testing.T) {
	start := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	if err := validateExplicitWindow(start, end, time.Time{}, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if err := validateExplicitWindow(time.Time{}, end, time.Time{}, time.Time{}, false); err == nil {
		t.Fatal("missing explicit start accepted")
	}
	if err := validateExplicitWindow(start, end, start.Add(time.Hour), end.Add(time.Hour), false); err == nil {
		t.Fatal("successful overlap accepted without rerun")
	}
	if err := validateExplicitWindow(start, end, start.Add(time.Hour), end.Add(time.Hour), true); err != nil {
		t.Fatalf("lineaged rerun overlap rejected: %v", err)
	}
}

func TestSchedulerHierarchyTickDispatchesChildrenThenParentIdempotently(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	store.addPolicy(t, "parent", policyFor("department:rd", true))
	store.addPolicy(t, "child-a", policyFor("project_group:a", false))
	store.addPolicy(t, "child-b", policyFor("project_group:b", false))
	store.spaces["department:rd"] = db.KnowledgeSpace{ID: "space-parent", EnterpriseID: "ent-1", Kind: "department", OrgScope: "department:rd", OrgVersion: 9}
	store.spaces["project_group:a"] = db.KnowledgeSpace{ID: "space-a", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: "project_group:a", OrgVersion: 9}
	store.spaces["project_group:b"] = db.KnowledgeSpace{ID: "space-b", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: "project_group:b", OrgVersion: 9}
	store.children["department:rd"] = []db.ListDreamImmediateChildrenRow{
		childRow(store.spaces["project_group:a"], "space-parent", "department", "rd", "department:rd"),
		childRow(store.spaces["project_group:b"], "space-parent", "department", "rd", "department:rd"),
	}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)

	n, err := scheduler.Tick(context.Background(), "ent-1", now)
	if err != nil || n != 2 {
		t.Fatalf("first tick count=%d err=%v", n, err)
	}
	if got := bus.ids(); !reflect.DeepEqual(got, []string{runIDFor("child-a", 1, now, 0), runIDFor("child-b", 1, now, 0)}) {
		t.Fatalf("first dispatch=%v", got)
	}
	if n, err = scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 0 || len(bus.ids()) != 2 {
		t.Fatalf("duplicate tick count=%d dispatch=%v err=%v", n, bus.ids(), err)
	}
	store.setStatus(runIDFor("child-a", 1, now, 0), "succeeded")
	if n, err = scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 0 {
		t.Fatalf("one-child tick count=%d err=%v", n, err)
	}
	store.setStatus(runIDFor("child-b", 1, now, 0), "succeeded")
	if n, err = scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 1 {
		t.Fatalf("parent tick count=%d runs=%+v err=%v", n, store.runs, err)
	}
	parent := store.run(runIDFor("parent", 1, now, 0))
	if parent.Status != "pending" || string(parent.Coverage) != `{"expected_children":2,"completed_children":2,"input_count":0}` || string(parent.MissingInputs) != `[]` {
		t.Fatalf("parent snapshot=%+v", parent)
	}
}

func TestSchedulerHierarchyTickCreatesBoundedImmutableRetry(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	p.MaxAttempts = 2
	store.addPolicy(t, "child-a", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space-a", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 1 {
		t.Fatalf("initial count=%d err=%v", n, err)
	}
	store.setStatus(runIDFor("child-a", 1, now, 0), "failed")
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 1 {
		t.Fatalf("retry count=%d err=%v", n, err)
	}
	retry := store.run(runIDFor("child-a", 1, now, 1))
	if retry.Attempt != 2 || retry.RerunOfRunID.Valid {
		t.Fatalf("retry=%+v", retry)
	}
	store.setStatus(retry.ID, "failed")
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 0 {
		t.Fatalf("capped count=%d err=%v", n, err)
	}
}

func TestSchedulerHierarchyPublishedVersionSupersedesFailedOldAttempt(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	v1 := policyFor("project_group:a", false)
	v1.MaxAttempts = 3
	store.addPolicy(t, "child", v1)
	store.spaces[v1.OrgUnitID] = db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: v1.OrgUnitID, OrgVersion: 7}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if _, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil {
		t.Fatal(err)
	}
	v1ID := runIDFor("child", 1, now, 0)
	store.setStatus(v1ID, "failed")
	v2 := v1
	v2.MaxAttempts = 2
	raw, _ := json.Marshal(v2)
	store.versions["child"] = db.DreamPolicyVersion{PolicyID: "child", Version: 2, Definition: raw}
	store.policies[0].Draft = raw
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 1 {
		t.Fatalf("v2 count=%d err=%v", n, err)
	}
	v2Run := store.run(runIDFor("child", 2, now, 0))
	if v2Run.PolicyVersion != 2 || v2Run.Attempt != 1 || !v2Run.WindowStart.Time.Equal(store.run(v1ID).WindowStart.Time) || store.run(v1ID).Status != "failed" {
		t.Fatalf("v1=%+v v2=%+v", store.run(v1ID), v2Run)
	}
	store.setStatus(v2Run.ID, "succeeded")
	next := now.Add(24 * time.Hour)
	if n, err := scheduler.Tick(context.Background(), "ent-1", next); err != nil || n != 1 {
		t.Fatalf("next v2 count=%d err=%v", n, err)
	}
	if run := store.run(runIDFor("child", 2, next, 0)); run.PolicyVersion != 2 || !run.WindowStart.Time.Equal(now) {
		t.Fatalf("next v2=%+v", run)
	}
}

func TestSchedulerHierarchyTickPartialPolicyRecordsTerminalChildFailure(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	parent := policyFor("department:rd", true)
	child := policyFor("project_group:a", false)
	child.MaxAttempts = 1
	store.addPolicy(t, "parent", parent)
	store.addPolicy(t, "child", child)
	store.spaces[parent.OrgUnitID] = db.KnowledgeSpace{ID: "space-parent", EnterpriseID: "ent-1", Kind: "department", OrgScope: parent.OrgUnitID, OrgVersion: 1}
	store.spaces[child.OrgUnitID] = db.KnowledgeSpace{ID: "space-child", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: child.OrgUnitID, OrgVersion: 1}
	store.children[parent.OrgUnitID] = []db.ListDreamImmediateChildrenRow{childRow(store.spaces[child.OrgUnitID], "space-parent", "department", "rd", parent.OrgUnitID)}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if _, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil {
		t.Fatal(err)
	}
	store.setStatus(runIDFor("child", 1, now, 0), "failed")
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	run := store.run(runIDFor("parent", 1, now, 0))
	if string(run.MissingInputs) != `[{"source_type":"child_dream_summary","source_id":"project_group:a","reason":"failed"}]` {
		t.Fatalf("missing=%s", run.MissingInputs)
	}
}

func TestSchedulerHierarchyPublishFailureLeavesDurablePendingRun(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	store.addPolicy(t, "child", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{err: errors.New("publish failed")}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if _, err := scheduler.Tick(context.Background(), "ent-1", now); err == nil {
		t.Fatal("publish failure hidden")
	}
	id := runIDFor("child", 1, now, 0)
	if run := store.run(id); run.ID != id || run.Status != "pending" {
		t.Fatalf("durable run=%+v", run)
	}
	bus.err = nil
	if n, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil || n != 0 {
		t.Fatalf("duplicate count=%d err=%v", n, err)
	}
}

func TestSchedulerHierarchyManualRerunIsImmutableAndIdempotent(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	store.addPolicy(t, "child-a", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space-a", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	if _, err := scheduler.Tick(context.Background(), "ent-1", now); err != nil {
		t.Fatal(err)
	}
	originalID := runIDFor("child-a", 1, now, 0)
	store.setStatus(originalID, "succeeded")
	runID, err := scheduler.Rerun(context.Background(), "ent-1", originalID, "manual-rerun-1", "audit-rerun-1")
	if err != nil {
		t.Fatal(err)
	}
	rerun := store.run(runID)
	if !rerun.RerunOfRunID.Valid || rerun.RerunOfRunID.String != originalID || rerun.Attempt != 1 || rerun.IdempotencyKey != "manual-rerun-1" || rerun.ID == originalID {
		t.Fatalf("rerun=%+v", rerun)
	}
	again, err := scheduler.Rerun(context.Background(), "ent-1", originalID, "manual-rerun-1", "audit-rerun-1")
	if err != nil || again != runID {
		t.Fatalf("duplicate rerun=%s err=%v", again, err)
	}
}

func TestSchedulerHierarchyBackfillRequiresBoundsAndSuccessfulOverlapLineage(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	store.addPolicy(t, "child-a", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space-a", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	start := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	first, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child-a", WindowStart: start, WindowEnd: end, IdempotencyKey: "backfill-1", AuditRefID: "audit-backfill-1"})
	if err != nil {
		t.Fatal(err)
	}
	store.setStatus(first, "succeeded")
	if _, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child-a", WindowStart: start.Add(time.Hour), WindowEnd: end.Add(time.Hour), IdempotencyKey: "backfill-overlap", AuditRefID: "audit-backfill-overlap"}); err == nil {
		t.Fatal("successful overlap accepted without rerun lineage")
	}
	lineaged, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child-a", WindowStart: start, WindowEnd: end, RerunOfRunID: first, IdempotencyKey: "backfill-rerun", AuditRefID: "audit-backfill-rerun"})
	if err != nil || !store.run(lineaged).RerunOfRunID.Valid {
		t.Fatalf("lineaged=%s run=%+v err=%v", lineaged, store.run(lineaged), err)
	}
}

func TestSchedulerHierarchyExplicitIdempotencyBindsCanonicalRequest(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	store.addPolicy(t, "child-a", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space-a", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	other := policyFor("project_group:b", false)
	store.addPolicy(t, "child-b", other)
	store.spaces[other.OrgUnitID] = db.KnowledgeSpace{ID: "space-b", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: other.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	clock := func() time.Time { return time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) }
	scheduler := NewScheduler(store, NewPolicyService(store), runner, WithSchedulerClock(clock))
	start := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	req := BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child-a", WindowStart: start, WindowEnd: start.Add(24 * time.Hour), IdempotencyKey: "explicit-key", AuditRefID: "audit-explicit-key"}
	first, err := scheduler.Backfill(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := scheduler.Backfill(context.Background(), req)
	if err != nil || again != first {
		t.Fatalf("same request=%s err=%v", again, err)
	}
	changed := req
	changed.WindowStart = changed.WindowStart.Add(-time.Hour)
	if _, err := scheduler.Backfill(context.Background(), changed); err == nil {
		t.Fatal("same key with changed window accepted")
	}
	otherReq := req
	otherReq.PolicyID = "child-b"
	if _, err := scheduler.Backfill(context.Background(), otherReq); err == nil {
		t.Fatal("same enterprise key in another org accepted")
	}
}

func TestSchedulerHierarchyBackfillRejectsFutureAndUnrelatedLineageWithoutMovingCursor(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	p := policyFor("project_group:a", false)
	store.addPolicy(t, "child", p)
	store.spaces[p.OrgUnitID] = db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: p.OrgUnitID, OrgVersion: 1}
	bus := &recordingSchedulerBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(JobTypeDream)
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	scheduler := NewScheduler(store, NewPolicyService(store), runner, WithSchedulerClock(func() time.Time { return now }))
	if _, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child", WindowStart: now, WindowEnd: now.Add(time.Hour), IdempotencyKey: "future", AuditRefID: "audit-future"}); err == nil {
		t.Fatal("future backfill accepted")
	}
	start := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	successful, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child", WindowStart: start, WindowEnd: start.Add(24 * time.Hour), IdempotencyKey: "success", AuditRefID: "audit-success"})
	if err != nil {
		t.Fatal(err)
	}
	store.setStatus(successful, "succeeded")
	unrelatedStart := start.Add(-48 * time.Hour)
	unrelated, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child", WindowStart: unrelatedStart, WindowEnd: unrelatedStart.Add(24 * time.Hour), IdempotencyKey: "unrelated", AuditRefID: "audit-unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	store.setStatus(unrelated, "failed")
	if _, err := scheduler.Backfill(context.Background(), BackfillRequest{EnterpriseID: "ent-1", PolicyID: "child", WindowStart: start, WindowEnd: start.Add(24 * time.Hour), RerunOfRunID: unrelated, IdempotencyKey: "bad-lineage", AuditRefID: "audit-bad-lineage"}); err == nil {
		t.Fatal("unrelated failed lineage bypassed overlap")
	}
	// Historical explicit runs never become the automatic schedule cursor.
	tickNow := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	if n, err := scheduler.Tick(context.Background(), "ent-1", tickNow); err != nil || n != 1 {
		t.Fatalf("tick count=%d err=%v", n, err)
	}
	automatic := store.run(runIDFor("child", 1, tickNow, 0))
	if !automatic.WindowStart.Time.Equal(time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("automatic cursor corrupted: %+v", automatic)
	}
}

func TestSchedulerHierarchyRejectsMixedOrgTreeVersion(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	store.pinnedOrgVersion = 9
	parent := policyFor("department:rd", true)
	child := policyFor("project_group:a", false)
	store.addPolicy(t, "parent", parent)
	store.addPolicy(t, "child", child)
	store.spaces[parent.OrgUnitID] = db.KnowledgeSpace{ID: "parent-space", EnterpriseID: "ent-1", Kind: "department", OrgScope: parent.OrgUnitID, OrgVersion: 9}
	store.spaces[child.OrgUnitID] = db.KnowledgeSpace{ID: "child-space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: child.OrgUnitID, OrgVersion: 10}
	store.children[parent.OrgUnitID] = []db.ListDreamImmediateChildrenRow{childRow(store.spaces[child.OrgUnitID], "parent-space", "department", "rd", parent.OrgUnitID)}
	runner := tasks.NewRunner(&recordingSchedulerBus{})
	runner.AllowEnqueue(JobTypeDream)
	scheduler := NewScheduler(store, NewPolicyService(store), runner)
	if _, err := scheduler.Tick(context.Background(), "ent-1", time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("mixed-version tree accepted")
	}
}

func TestSchedulerHierarchyRejectsConcurrentOrgUpdateAfterVersionPin(t *testing.T) {
	store := newSchedulerHierarchyStore(t)
	store.pinnedOrgVersion = 9
	parent := policyFor("department:rd", true)
	child := policyFor("project_group:a", false)
	store.addPolicy(t, "parent", parent)
	store.addPolicy(t, "child", child)
	store.spaces[parent.OrgUnitID] = db.KnowledgeSpace{ID: "parent-space", EnterpriseID: "ent-1", Kind: "department", OrgScope: parent.OrgUnitID, OrgVersion: 9}
	store.spaces[child.OrgUnitID] = db.KnowledgeSpace{ID: "child-space", EnterpriseID: "ent-1", Kind: "project_group", OrgScope: child.OrgUnitID, OrgVersion: 9}
	store.children[parent.OrgUnitID] = []db.ListDreamImmediateChildrenRow{childRow(store.spaces[child.OrgUnitID], "parent-space", "department", "rd", parent.OrgUnitID)}
	store.afterVersionPin = func() {
		updated := store.spaces[child.OrgUnitID]
		updated.OrgVersion = 10
		store.spaces[child.OrgUnitID] = updated
		store.children[parent.OrgUnitID] = []db.ListDreamImmediateChildrenRow{childRow(updated, "parent-space", "department", "rd", parent.OrgUnitID)}
	}
	runner := tasks.NewRunner(&recordingSchedulerBus{})
	runner.AllowEnqueue(JobTypeDream)
	if _, err := NewScheduler(store, NewPolicyService(store), runner).Tick(context.Background(), "ent-1", time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("org update after version pin was mixed into decision")
	}
}

func policyFor(org string, parent bool) Policy {
	p := validPolicy()
	p.OrgUnitID = org
	p.OutputSpaceID = "space-" + org
	p.MaxAttempts = 3
	p.AllowPartialChildren = parent
	if parent {
		p.InputSources = []sdkdream.Source{sdkdream.SourceChildDreamSummary}
	}
	return p
}

func childRow(space db.KnowledgeSpace, parentSpaceID, parentKind, parentID, parentScope string) db.ListDreamImmediateChildrenRow {
	return db.ListDreamImmediateChildrenRow{ID: space.ID, EnterpriseID: space.EnterpriseID, Kind: space.Kind, Name: space.Name, OrgScope: space.OrgScope, OrgVersion: space.OrgVersion, CreatedAt: space.CreatedAt, UpdatedAt: space.UpdatedAt, ParentSpaceID: parentSpaceID, ParentScopeKind: parentKind, ParentScopeID: parentID, ParentOrgScope: parentScope}
}

type recordingSchedulerBus struct {
	mu        sync.Mutex
	published []string
	err       error
}

func (b *recordingSchedulerBus) Publish(_ context.Context, _, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, id)
	return b.err
}
func (*recordingSchedulerBus) Subscribe(context.Context, string, func(context.Context, string)) (func(), error) {
	return func() {}, nil
}
func (b *recordingSchedulerBus) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

type schedulerHierarchyStore struct {
	SchedulerStore
	mu               sync.Mutex
	policies         []db.DreamPolicy
	versions         map[string]db.DreamPolicyVersion
	spaces           map[string]db.KnowledgeSpace
	children         map[string][]db.ListDreamImmediateChildrenRow
	runs             map[string]db.DreamRun
	pinnedOrgVersion int64
	afterVersionPin  func()
}

func (s *schedulerHierarchyStore) GetDreamOrgTreeVersion(context.Context, string) (int64, error) {
	if s.pinnedOrgVersion > 0 {
		version := s.pinnedOrgVersion
		if s.afterVersionPin != nil {
			hook := s.afterVersionPin
			s.afterVersionPin = nil
			hook()
		}
		return version, nil
	}
	var max int64
	for _, space := range s.spaces {
		if space.OrgVersion > max {
			max = space.OrgVersion
		}
	}
	if max == 0 {
		return 0, pgx.ErrNoRows
	}
	return max, nil
}

func newSchedulerHierarchyStore(t *testing.T) *schedulerHierarchyStore {
	t.Helper()
	return &schedulerHierarchyStore{versions: map[string]db.DreamPolicyVersion{}, spaces: map[string]db.KnowledgeSpace{}, children: map[string][]db.ListDreamImmediateChildrenRow{}, runs: map[string]db.DreamRun{}}
}
func (s *schedulerHierarchyStore) addPolicy(t *testing.T, id string, p Policy) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s.policies = append(s.policies, db.DreamPolicy{ID: id, EnterpriseID: "ent-1", OrgScope: p.OrgUnitID, Status: "published", Draft: raw})
	s.versions[id] = db.DreamPolicyVersion{PolicyID: id, Version: 1, Definition: raw}
}
func (s *schedulerHierarchyStore) ListPublishedDreamPolicies(context.Context, string) ([]db.DreamPolicy, error) {
	return append([]db.DreamPolicy(nil), s.policies...), nil
}
func (s *schedulerHierarchyStore) GetLatestDreamPolicyVersion(_ context.Context, id string) (db.DreamPolicyVersion, error) {
	row, ok := s.versions[id]
	if !ok {
		return db.DreamPolicyVersion{}, pgx.ErrNoRows
	}
	return row, nil
}
func (s *schedulerHierarchyStore) GetDreamPolicy(_ context.Context, id string) (db.DreamPolicy, error) {
	for _, row := range s.policies {
		if row.ID == id {
			return row, nil
		}
	}
	return db.DreamPolicy{}, pgx.ErrNoRows
}
func (s *schedulerHierarchyStore) GetDreamPolicyVersion(_ context.Context, arg db.GetDreamPolicyVersionParams) (db.DreamPolicyVersion, error) {
	row, ok := s.versions[arg.PolicyID]
	if !ok || row.Version != arg.Version {
		return db.DreamPolicyVersion{}, pgx.ErrNoRows
	}
	return row, nil
}
func (s *schedulerHierarchyStore) GetDreamRun(_ context.Context, id string) (db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.runs[id]
	if !ok {
		return db.DreamRun{}, pgx.ErrNoRows
	}
	return row, nil
}
func (s *schedulerHierarchyStore) GetDreamRunByIdempotencyKey(_ context.Context, arg db.GetDreamRunByIdempotencyKeyParams) (db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.runs {
		if row.EnterpriseID == arg.EnterpriseID && row.IdempotencyKey == arg.IdempotencyKey {
			return row, nil
		}
	}
	return db.DreamRun{}, pgx.ErrNoRows
}
func (s *schedulerHierarchyStore) GetKnowledgeSpaceByScope(_ context.Context, arg db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error) {
	row, ok := s.spaces[arg.OrgScope]
	if !ok || row.EnterpriseID != arg.EnterpriseID {
		return db.KnowledgeSpace{}, pgx.ErrNoRows
	}
	return row, nil
}
func (s *schedulerHierarchyStore) ListDreamImmediateChildren(_ context.Context, arg db.ListDreamImmediateChildrenParams) ([]db.ListDreamImmediateChildrenRow, error) {
	return append([]db.ListDreamImmediateChildrenRow(nil), s.children[arg.ParentScopeKind+":"+arg.ParentScopeID]...), nil
}
func (s *schedulerHierarchyStore) ListDreamRunsByOrg(_ context.Context, arg db.ListDreamRunsByOrgParams) ([]db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []db.DreamRun
	for _, row := range s.runs {
		if row.EnterpriseID == arg.EnterpriseID && row.OrgUnitID == arg.OrgUnitID {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].WindowEnd.Time.Equal(rows[j].WindowEnd.Time) {
			return rows[i].WindowEnd.Time.After(rows[j].WindowEnd.Time)
		}
		return rows[i].ID > rows[j].ID
	})
	if len(rows) > int(arg.ResultLimit) {
		rows = rows[:arg.ResultLimit]
	}
	return rows, nil
}
func (s *schedulerHierarchyStore) GetLatestDreamRunForPolicy(_ context.Context, id string) (db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []db.DreamRun
	for _, row := range s.runs {
		if row.PolicyID == id && (row.OperationKind == "" || row.OperationKind == "scheduled" || row.OperationKind == "automatic_retry") && !row.RerunOfRunID.Valid {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return db.DreamRun{}, pgx.ErrNoRows
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].WindowEnd.Time.Equal(rows[j].WindowEnd.Time) {
			return rows[i].WindowEnd.Time.After(rows[j].WindowEnd.Time)
		}
		return rows[i].Attempt > rows[j].Attempt
	})
	return rows[0], nil
}
func (s *schedulerHierarchyStore) GetLatestDreamRunForPolicyVersion(ctx context.Context, arg db.GetLatestDreamRunForPolicyVersionParams) (db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []db.DreamRun
	for _, row := range s.runs {
		if row.PolicyID == arg.PolicyID && row.PolicyVersion == arg.PolicyVersion && (row.OperationKind == "" || row.OperationKind == "scheduled" || row.OperationKind == "automatic_retry") && !row.RerunOfRunID.Valid {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return db.DreamRun{}, pgx.ErrNoRows
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].WindowEnd.Time.Equal(rows[j].WindowEnd.Time) {
			return rows[i].WindowEnd.Time.After(rows[j].WindowEnd.Time)
		}
		return rows[i].Attempt > rows[j].Attempt
	})
	return rows[0], nil
}
func (s *schedulerHierarchyStore) CreateDreamRun(_ context.Context, arg db.CreateDreamRunParams) (db.DreamRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[arg.ID]; ok {
		return db.DreamRun{}, &pgconn.PgError{Code: "23505"}
	}
	for _, existing := range s.runs {
		if existing.EnterpriseID == arg.EnterpriseID && existing.IdempotencyKey == arg.IdempotencyKey {
			return db.DreamRun{}, &pgconn.PgError{Code: "23505"}
		}
	}
	row := db.DreamRun{ID: arg.ID, PolicyID: arg.PolicyID, Version: arg.Version, EnterpriseID: arg.EnterpriseID, Status: arg.Status, WindowStart: arg.WindowStart, WindowEnd: arg.WindowEnd, OrgUnitID: arg.OrgUnitID, PolicyVersion: arg.PolicyVersion, WorkflowID: pgtype.Text{String: arg.WorkflowID, Valid: true}, WorkflowVersion: pgtype.Int4{Int32: arg.WorkflowVersion, Valid: true}, Timezone: arg.Timezone, InputSnapshot: arg.InputSnapshot, VisibilitySnapshot: arg.VisibilitySnapshot, ModelRoute: arg.ModelRoute, ModelVersion: arg.ModelVersion, Attempt: arg.Attempt, RerunOfRunID: arg.RerunOfRunID, Coverage: arg.Coverage, MissingInputs: arg.MissingInputs, IdempotencyKey: arg.IdempotencyKey, OrgVersion: arg.OrgVersion, OperationKind: arg.OperationKind, AuditRefID: arg.AuditRefID}
	s.runs[row.ID] = row
	return row, nil
}
func (s *schedulerHierarchyStore) setStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.runs[id]
	row.Status = status
	s.runs[id] = row
}
func (s *schedulerHierarchyStore) run(id string) db.DreamRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func stringSet(values ...string) []string { return values }
