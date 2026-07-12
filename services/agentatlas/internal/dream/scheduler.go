package dream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

const JobTypeDream = "dream_run"

// SchedulerStore extends PolicyStore with run bookkeeping.
type SchedulerStore interface {
	PolicyStore
	CreateDreamRun(ctx context.Context, arg db.CreateDreamRunParams) (db.CreateDreamRunRow, error)
	GetDreamRun(ctx context.Context, id string) (db.DreamRun, error)
	GetLatestDreamRunForPolicy(ctx context.Context, policyID string) (db.DreamRun, error)
	GetLatestDreamRunForPolicyVersion(ctx context.Context, arg db.GetLatestDreamRunForPolicyVersionParams) (db.DreamRun, error)
	GetKnowledgeSpaceByScope(ctx context.Context, arg db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error)
	ListDreamImmediateChildren(ctx context.Context, arg db.ListDreamImmediateChildrenParams) ([]db.ListDreamImmediateChildrenRow, error)
	ListDreamRunsByOrg(ctx context.Context, arg db.ListDreamRunsByOrgParams) ([]db.DreamRun, error)
	GetDreamRunByIdempotencyKey(ctx context.Context, arg db.GetDreamRunByIdempotencyKeyParams) (db.DreamRun, error)
	GetDreamOrgTreeVersion(ctx context.Context, enterpriseID string) (int64, error)
}

// BackfillRequest is intentionally bounded and explicit. IdempotencyKey is a
// caller-owned operation identity; RerunOfRunID is required to overlap a
// successful historical window.
type BackfillRequest struct {
	EnterpriseID   string
	PolicyID       string
	WindowStart    time.Time
	WindowEnd      time.Time
	RerunOfRunID   string
	IdempotencyKey string
	AuditRefID     string
}

// Scheduler turns published policies into due dream runs. Run ids are
// deterministic per (policy, window) so double ticks stay idempotent.
type Scheduler struct {
	store            SchedulerStore
	policy           *PolicyService
	runner           *tasks.Runner
	clock            func() time.Time
	inTransaction    bool
	deferredDispatch *[]string
}

type SchedulerOption func(*Scheduler)

func WithSchedulerClock(clock func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if clock != nil {
			s.clock = clock
		}
	}
}

func NewScheduler(store SchedulerStore, policy *PolicyService, runner *tasks.Runner, options ...SchedulerOption) *Scheduler {
	s := &Scheduler{store: store, policy: policy, runner: runner, clock: time.Now}
	for _, option := range options {
		option(s)
	}
	return s
}

// runIDFor is deterministic across schedulers. Sequence zero is the original
// scheduled window; retries and explicit reruns use immutable later sequences.
func runIDFor(policyID string, policyVersion int32, windowEnd time.Time, rerunSequence int32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%d", policyID, policyVersion, windowEnd.UTC().Format(time.RFC3339Nano), rerunSequence)))
	return "dr_" + hex.EncodeToString(sum[:8])
}

// Due computes whether a policy owes a run at 'now', returning the window.
// The window closes at the most recent schedule firing <= now and opens at
// the previous firing (or 24h before for the first run).
func Due(p Policy, lastWindowEnd time.Time, now time.Time) (start, end time.Time, due bool, err error) {
	sched, err := cronParser.Parse(p.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	location, err := dreamLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	localNow := now.In(location)
	if !lastWindowEnd.IsZero() {
		localLast := lastWindowEnd.In(location)
		lastFire := sched.Next(localLast)
		if lastFire.After(localNow) {
			return time.Time{}, time.Time{}, false, nil
		}
		for next := sched.Next(lastFire); !next.After(localNow); next = sched.Next(next) {
			lastFire = next
		}
		return lastWindowEnd, lastFire.UTC(), true, nil
	}

	// Find both firings that define the first exact schedule window. Expanding
	// horizons avoid a minute-by-minute multi-year scan for normal schedules.
	var previousFire, lastFire time.Time
	for _, horizon := range []time.Duration{48 * time.Hour, 8 * 24 * time.Hour, 32 * 24 * time.Hour, 370 * 24 * time.Hour, 5 * 370 * 24 * time.Hour} {
		previousFire, lastFire = time.Time{}, time.Time{}
		for firing := sched.Next(localNow.Add(-horizon)); !firing.After(localNow); firing = sched.Next(firing) {
			previousFire, lastFire = lastFire, firing
		}
		if !previousFire.IsZero() {
			break
		}
	}
	if lastFire.IsZero() {
		return time.Time{}, time.Time{}, false, nil // next firing is in the future
	}
	if previousFire.IsZero() {
		return time.Time{}, time.Time{}, false, fmt.Errorf("Dream schedule has no bounded previous firing")
	}
	return previousFire.UTC(), lastFire.UTC(), true, nil
}

// Tick scans published policies of one enterprise and dispatches due runs.
func (s *Scheduler) Tick(ctx context.Context, enterpriseID string, now time.Time) (int, error) {
	var dispatched int
	if ids, handled, err := s.inOrgSnapshot(ctx, func(txScheduler *Scheduler) error {
		var err error
		dispatched, err = txScheduler.Tick(ctx, enterpriseID, now)
		return err
	}); handled {
		if err != nil {
			return dispatched, err
		}
		if err := s.publishDeferred(ctx, ids, "dispatch dream run"); err != nil {
			return dispatched, err
		}
		return dispatched, nil
	}
	orgVersion, err := s.store.GetDreamOrgTreeVersion(ctx, enterpriseID)
	if err != nil {
		return 0, fmt.Errorf("pin Dream org tree version: %w", err)
	}
	if orgVersion < 1 {
		return 0, fmt.Errorf("Dream org tree has no versioned spaces")
	}
	policies, err := s.store.ListPublishedDreamPolicies(ctx, enterpriseID)
	if err != nil {
		return 0, fmt.Errorf("list policies: %w", err)
	}
	if len(policies) > maxHierarchyPolicies {
		return 0, fmt.Errorf("published Dream policies exceed bound %d", maxHierarchyPolicies)
	}
	candidates := make([]HierarchyCandidate, 0, len(policies))
	byOrg := make(map[string]*HierarchyCandidate, len(policies))
	for _, row := range policies {
		p, version, err := s.policy.LoadPublished(ctx, row.ID)
		if err != nil {
			return 0, err
		}
		space, err := s.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{EnterpriseID: enterpriseID, OrgScope: p.OrgUnitID})
		if err != nil {
			return 0, fmt.Errorf("load Dream hierarchy scope %s: %w", p.OrgUnitID, err)
		}
		if space.EnterpriseID != enterpriseID || space.OrgScope != p.OrgUnitID || space.Kind == "" || space.OrgVersion < 1 || space.OrgVersion > orgVersion {
			return 0, fmt.Errorf("Dream hierarchy scope %s has invalid versioned provenance", p.OrgUnitID)
		}
		candidate := HierarchyCandidate{PolicyID: row.ID, OrgUnitID: p.OrgUnitID, policy: p, version: version}
		if hasDreamSource(p.InputSources, sdkdream.SourceChildDreamSummary) {
			ref, ok := parseScopeRef(p.OrgUnitID)
			if !ok {
				return 0, fmt.Errorf("Dream hierarchy scope %s is invalid", p.OrgUnitID)
			}
			children, err := s.store.ListDreamImmediateChildren(ctx, db.ListDreamImmediateChildrenParams{EnterpriseID: enterpriseID, ParentScopeKind: space.Kind, ParentScopeID: ref.id, ResultLimit: maxHierarchyChildren + 1})
			if err != nil {
				return 0, fmt.Errorf("list Dream children for %s: %w", p.OrgUnitID, err)
			}
			if len(children) > int(maxHierarchyChildren) {
				return 0, fmt.Errorf("Dream hierarchy children exceed bound %d", maxHierarchyChildren)
			}
			for _, child := range children {
				if child.OrgVersion > orgVersion {
					return 0, fmt.Errorf("Dream child %s exceeds pinned org version %d", child.OrgScope, orgVersion)
				}
				if child.EnterpriseID == enterpriseID && child.ParentOrgScope == p.OrgUnitID && child.OrgScope != "" {
					candidate.children = append(candidate.children, child.OrgScope)
				}
			}
			sort.Strings(candidate.children)
		}
		candidates = append(candidates, candidate)
	}
	for i := range candidates {
		if _, duplicate := byOrg[candidates[i].OrgUnitID]; duplicate {
			return 0, fmt.Errorf("multiple published Dream policies target %s", candidates[i].OrgUnitID)
		}
		byOrg[candidates[i].OrgUnitID] = &candidates[i]
	}
	for pass := 0; pass < len(candidates); pass++ {
		changed := false
		for i := range candidates {
			for _, child := range candidates[i].children {
				if childCandidate := byOrg[child]; childCandidate != nil && childCandidate.Depth <= candidates[i].Depth {
					childCandidate.Depth = candidates[i].Depth + 1
					changed = true
				}
			}
		}
		if !changed {
			break
		}
		if pass == len(candidates)-1 {
			return 0, fmt.Errorf("Dream hierarchy contains a policy cycle")
		}
	}
	sortHierarchyCandidates(candidates)
	for key := range byOrg {
		delete(byOrg, key)
	}
	for i := range candidates {
		byOrg[candidates[i].OrgUnitID] = &candidates[i]
	}

	dispatched = 0
	for _, candidate := range candidates {
		p, version, row := candidate.policy, candidate.version, candidate
		var lastEnd time.Time
		last, err := s.store.GetLatestDreamRunForPolicyVersion(ctx, db.GetLatestDreamRunForPolicyVersionParams{PolicyID: row.PolicyID, PolicyVersion: version})
		currentVersionRun := err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			last, err = s.store.GetLatestDreamRunForPolicy(ctx, row.PolicyID)
		}
		switch {
		case err == nil:
			lastEnd = last.WindowEnd.Time
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return dispatched, err
		}
		var start, end time.Time
		var attempt, sequence int32 = 1, 0
		operationKind := "scheduled"
		if err == nil {
			if !currentVersionRun && last.Status == "failed" && last.PolicyVersion != version {
				start, end = last.WindowStart.Time, last.WindowEnd.Time
			} else if next, retry := retryAttempt(last.Status, last.Attempt, p.MaxAttempts); retry && last.PolicyVersion == version {
				start, end, attempt, sequence = last.WindowStart.Time, last.WindowEnd.Time, next, next-1
				operationKind = "automatic_retry"
			} else {
				if last.Status == "pending" || last.Status == "running" || last.Status == "waiting_confirmation" || (last.Status == "failed" && last.Attempt < p.MaxAttempts) {
					continue
				}
				var due bool
				start, end, due, err = Due(p, lastEnd, now)
				if err != nil {
					return dispatched, err
				}
				if !due {
					continue
				}
			}
		} else {
			var due bool
			start, end, due, err = Due(p, time.Time{}, now)
			if err != nil {
				return dispatched, err
			}
			if !due {
				continue
			}
		}

		explicit := make([]MissingInput, 0)
		states := make([]ChildRunState, 0, len(row.children))
		maxAttempts := make(map[string]int32, len(row.children))
		for _, child := range row.children {
			childPolicy := byOrg[child]
			if childPolicy == nil {
				explicit = append(explicit, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child, Reason: sdkdream.MissingNotFound})
				continue
			}
			maxAttempts[child] = childPolicy.policy.MaxAttempts
			runs, err := s.listBoundedRuns(ctx, enterpriseID, child)
			if err != nil {
				return dispatched, fmt.Errorf("list child Dream runs: %w", err)
			}
			for _, childRun := range runs {
				if childRun.PolicyID == childPolicy.PolicyID && childRun.WindowStart.Valid && childRun.WindowEnd.Valid && childRun.WindowStart.Time.Equal(start) && childRun.WindowEnd.Time.Equal(end) {
					states = append(states, ChildRunState{OrgUnitID: child, Status: childRun.Status, Attempt: childRun.Attempt})
				}
			}
		}
		ready, coverage, missing, err := childReadiness(row.children, states, explicit, p.AllowPartialChildren, maxAttempts)
		if err != nil {
			return dispatched, err
		}
		if !ready {
			continue
		}
		runID := runIDFor(row.PolicyID, version, end, sequence)
		sourceCounts := make([]map[string]any, 0, len(p.InputSources))
		for _, source := range p.InputSources {
			sourceCounts = append(sourceCounts, map[string]any{"source_type": source, "count": 0})
		}
		inputSnapshot, err := json.Marshal(map[string]any{
			"source_counts": sourceCounts, "sanitized_input_ids": []string{},
		})
		if err != nil {
			return dispatched, fmt.Errorf("snapshot Dream inputs: %w", err)
		}
		visibilitySnapshot, err := json.Marshal(map[string]any{
			"visibility_level": p.VisibilityLevel, "org_unit_ids": []string{p.OrgUnitID},
			"masked_field_count": 0,
		})
		if err != nil {
			return dispatched, fmt.Errorf("snapshot Dream visibility: %w", err)
		}
		coverageJSON, err := json.Marshal(coverage)
		if err != nil {
			return dispatched, err
		}
		missingJSON, err := json.Marshal(missing)
		if err != nil {
			return dispatched, err
		}
		if _, err := s.store.CreateDreamRun(ctx, db.CreateDreamRunParams{
			ID: runID, PolicyID: row.PolicyID, Version: version,
			EnterpriseID: enterpriseID, Status: "pending",
			WindowStart: pgtype.Timestamptz{Time: start, Valid: true},
			WindowEnd:   pgtype.Timestamptz{Time: end, Valid: true},
			OrgUnitID:   p.OrgUnitID, PolicyVersion: version,
			WorkflowID: p.Workflow.ID, WorkflowVersion: p.Workflow.Version,
			Timezone: p.Timezone, InputSnapshot: inputSnapshot,
			VisibilitySnapshot: visibilitySnapshot,
			ModelRoute:         fmt.Sprintf("workflow/%s", p.Workflow.ID),
			ModelVersion:       fmt.Sprintf("v%d", p.Workflow.Version), Attempt: attempt,
			Coverage: coverageJSON, MissingInputs: missingJSON, IdempotencyKey: runID,
			OrgVersion: orgVersion, OperationKind: operationKind,
		}); err != nil {
			// Only a duplicate PK means another scheduler already created this
			// window's run; any other error is a real failure — fail loud, do
			// not report the window as dispatched.
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				continue
			}
			return dispatched, fmt.Errorf("create dream run %s: %w", runID, err)
		}
		if err := s.dispatch(ctx, runID); err != nil {
			return dispatched, fmt.Errorf("dispatch dream run: %w", err)
		}
		dispatched++
	}
	return dispatched, nil
}

const (
	maxHierarchyPolicies       = 1000
	maxHierarchyChildren int32 = 1000
	maxHierarchyRuns           = 1000
)

func hasDreamSource(sources []sdkdream.Source, wanted sdkdream.Source) bool {
	for _, source := range sources {
		if source == wanted {
			return true
		}
	}
	return false
}

// Rerun creates a new immutable run pinned to the original policy/workflow
// version and exact window. The original row is never mutated.
func (s *Scheduler) Rerun(ctx context.Context, enterpriseID, runID, idempotencyKey, auditRefID string) (string, error) {
	var id string
	if ids, handled, err := s.inOrgSnapshot(ctx, func(txScheduler *Scheduler) error {
		var err error
		id, err = txScheduler.Rerun(ctx, enterpriseID, runID, idempotencyKey, auditRefID)
		return err
	}); handled {
		if err != nil {
			return id, err
		}
		if err := s.publishDeferred(ctx, ids, "dispatch explicit Dream run"); err != nil {
			return id, err
		}
		return id, nil
	}
	original, err := s.store.GetDreamRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load Dream rerun source: %w", err)
	}
	if original.EnterpriseID != enterpriseID {
		return "", fmt.Errorf("Dream rerun source is outside enterprise")
	}
	policy, err := s.policy.LoadVersion(ctx, original.PolicyID, original.PolicyVersion)
	if err != nil {
		return "", err
	}
	return s.createExplicitRun(ctx, BackfillRequest{EnterpriseID: enterpriseID, PolicyID: original.PolicyID, WindowStart: original.WindowStart.Time, WindowEnd: original.WindowEnd.Time, RerunOfRunID: original.ID, IdempotencyKey: idempotencyKey, AuditRefID: auditRefID}, policy, original.PolicyVersion, "manual_rerun")
}

func (s *Scheduler) LookupRerun(ctx context.Context, enterpriseID, sourceRunID, key string) (string, bool, error) {
	row, err := s.store.GetDreamRunByIdempotencyKey(ctx, db.GetDreamRunByIdempotencyKeyParams{EnterpriseID: enterpriseID, IdempotencyKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if row.EnterpriseID != enterpriseID || row.OperationKind != "manual_rerun" || !row.RerunOfRunID.Valid || row.RerunOfRunID.String != sourceRunID {
		return "", false, fmt.Errorf("Dream idempotency key is already bound to a different canonical request")
	}
	return row.ID, true, nil
}

// Backfill schedules exactly one caller-bounded historical window.
func (s *Scheduler) Backfill(ctx context.Context, req BackfillRequest) (string, error) {
	var id string
	if ids, handled, err := s.inOrgSnapshot(ctx, func(txScheduler *Scheduler) error {
		var err error
		id, err = txScheduler.Backfill(ctx, req)
		return err
	}); handled {
		if err != nil {
			return id, err
		}
		if err := s.publishDeferred(ctx, ids, "dispatch explicit Dream run"); err != nil {
			return id, err
		}
		return id, nil
	}
	if req.EnterpriseID == "" || req.PolicyID == "" || req.IdempotencyKey == "" || req.AuditRefID == "" {
		return "", fmt.Errorf("Dream backfill requires enterprise, policy, and idempotency key")
	}
	if req.WindowEnd.After(s.clock()) {
		return "", fmt.Errorf("Dream backfill window must be historical")
	}
	policy, version, err := s.policy.LoadPublished(ctx, req.PolicyID)
	if err != nil {
		return "", err
	}
	policyRow, err := s.store.GetDreamPolicy(ctx, req.PolicyID)
	if err != nil || policyRow.EnterpriseID != req.EnterpriseID {
		return "", fmt.Errorf("Dream backfill policy is outside enterprise")
	}
	runs, err := s.listBoundedRuns(ctx, req.EnterpriseID, policy.OrgUnitID)
	if err != nil {
		return "", fmt.Errorf("list Dream backfill windows: %w", err)
	}
	var successfulStart, successfulEnd time.Time
	for _, run := range runs {
		if run.PolicyID == req.PolicyID && run.Status == "succeeded" && run.WindowStart.Valid && run.WindowEnd.Valid && req.WindowStart.Before(run.WindowEnd.Time) && run.WindowStart.Time.Before(req.WindowEnd) {
			successfulStart, successfulEnd = run.WindowStart.Time, run.WindowEnd.Time
			break
		}
	}
	if err := validateExplicitWindow(req.WindowStart, req.WindowEnd, successfulStart, successfulEnd, req.RerunOfRunID != ""); err != nil {
		return "", err
	}
	if req.RerunOfRunID != "" {
		original, err := s.store.GetDreamRun(ctx, req.RerunOfRunID)
		if err != nil || original.EnterpriseID != req.EnterpriseID || original.PolicyID != req.PolicyID || original.OrgUnitID != policy.OrgUnitID || original.Status != "succeeded" || !original.WindowStart.Valid || !original.WindowEnd.Valid || !original.WindowStart.Time.Equal(req.WindowStart) || !original.WindowEnd.Time.Equal(req.WindowEnd) {
			return "", fmt.Errorf("Dream backfill rerun lineage is invalid")
		}
	}
	return s.createExplicitRun(ctx, req, policy, version, "backfill")
}

func (s *Scheduler) LookupBackfill(ctx context.Context, req BackfillRequest) (string, bool, error) {
	row, err := s.store.GetDreamRunByIdempotencyKey(ctx, db.GetDreamRunByIdempotencyKeyParams{EnterpriseID: req.EnterpriseID, IdempotencyKey: req.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	rerunMatches := row.RerunOfRunID.Valid == (req.RerunOfRunID != "") && (!row.RerunOfRunID.Valid || row.RerunOfRunID.String == req.RerunOfRunID)
	if row.EnterpriseID != req.EnterpriseID || row.PolicyID != req.PolicyID || row.OperationKind != "backfill" || !row.WindowStart.Valid || !row.WindowEnd.Valid || !row.WindowStart.Time.Equal(req.WindowStart) || !row.WindowEnd.Time.Equal(req.WindowEnd) || !rerunMatches {
		return "", false, fmt.Errorf("Dream idempotency key is already bound to a different canonical request")
	}
	return row.ID, true, nil
}

func (s *Scheduler) createExplicitRun(ctx context.Context, req BackfillRequest, p Policy, version int32, operationKind string) (string, error) {
	if err := validateExplicitWindow(req.WindowStart, req.WindowEnd, time.Time{}, time.Time{}, req.RerunOfRunID != ""); err != nil {
		return "", err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return "", fmt.Errorf("Dream explicit run idempotency key is required and bounded")
	}
	existing, existingErr := s.store.GetDreamRunByIdempotencyKey(ctx, db.GetDreamRunByIdempotencyKeyParams{EnterpriseID: req.EnterpriseID, IdempotencyKey: req.IdempotencyKey})
	if existingErr == nil {
		return validateExplicitIdempotency(existing, req, p, version, operationKind)
	}
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return "", existingErr
	}
	runs, err := s.listBoundedRuns(ctx, req.EnterpriseID, p.OrgUnitID)
	if err != nil {
		return "", err
	}
	sequence := int32(0)
	for _, run := range runs {
		if run.PolicyID == req.PolicyID && run.PolicyVersion == version && run.WindowEnd.Valid && run.WindowEnd.Time.Equal(req.WindowEnd) {
			sequence++
		}
	}
	inputSnapshot, visibilitySnapshot, err := schedulerSnapshots(p)
	if err != nil {
		return "", err
	}
	orgVersion, err := s.store.GetDreamOrgTreeVersion(ctx, req.EnterpriseID)
	if err != nil {
		return "", fmt.Errorf("pin Dream org tree version: %w", err)
	}
	if orgVersion < 1 {
		return "", fmt.Errorf("Dream org tree has no versioned spaces")
	}
	coverage, missing, err := s.explicitHierarchySnapshot(ctx, req, p, orgVersion)
	if err != nil {
		return "", err
	}
	coverageJSON, err := json.Marshal(coverage)
	if err != nil {
		return "", err
	}
	missingJSON, err := json.Marshal(missing)
	if err != nil {
		return "", err
	}
	rerun := pgtype.Text{}
	if req.RerunOfRunID != "" {
		rerun = pgtype.Text{String: req.RerunOfRunID, Valid: true}
	}
	for collision := int32(0); collision < 20; collision++ {
		candidateSequence := sequence + collision
		id := runIDFor(req.PolicyID, version, req.WindowEnd, candidateSequence)
		_, err := s.store.CreateDreamRun(ctx, db.CreateDreamRunParams{ID: id, PolicyID: req.PolicyID, Version: version, EnterpriseID: req.EnterpriseID, Status: "pending", WindowStart: pgtype.Timestamptz{Time: req.WindowStart, Valid: true}, WindowEnd: pgtype.Timestamptz{Time: req.WindowEnd, Valid: true}, OrgUnitID: p.OrgUnitID, PolicyVersion: version, WorkflowID: p.Workflow.ID, WorkflowVersion: p.Workflow.Version, Timezone: p.Timezone, InputSnapshot: inputSnapshot, VisibilitySnapshot: visibilitySnapshot, ModelRoute: "workflow/" + p.Workflow.ID, ModelVersion: fmt.Sprintf("v%d", p.Workflow.Version), Attempt: 1, RerunOfRunID: rerun, Coverage: coverageJSON, MissingInputs: missingJSON, IdempotencyKey: req.IdempotencyKey, OrgVersion: orgVersion, OperationKind: operationKind, AuditRefID: pgtype.Text{String: req.AuditRefID, Valid: true}})
		if err == nil {
			if err := s.dispatch(ctx, id); err != nil {
				return id, fmt.Errorf("dispatch explicit Dream run: %w", err)
			}
			return id, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			existing, listErr := s.store.GetDreamRunByIdempotencyKey(ctx, db.GetDreamRunByIdempotencyKeyParams{EnterpriseID: req.EnterpriseID, IdempotencyKey: req.IdempotencyKey})
			if listErr == nil {
				return validateExplicitIdempotency(existing, req, p, version, operationKind)
			}
			if !errors.Is(listErr, pgx.ErrNoRows) {
				return "", listErr
			}
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			return "", err
		}
		existing, listErr := s.store.GetDreamRunByIdempotencyKey(ctx, db.GetDreamRunByIdempotencyKeyParams{EnterpriseID: req.EnterpriseID, IdempotencyKey: req.IdempotencyKey})
		if listErr == nil {
			return validateExplicitIdempotency(existing, req, p, version, operationKind)
		}
		if !errors.Is(listErr, pgx.ErrNoRows) {
			return "", listErr
		}
	}
	return "", fmt.Errorf("Dream explicit run sequence collision bound exceeded")
}

func (s *Scheduler) dispatch(ctx context.Context, id string) error {
	if s.deferredDispatch != nil {
		*s.deferredDispatch = append(*s.deferredDispatch, id)
		return nil
	}
	return s.runner.Enqueue(ctx, JobTypeDream, id)
}

func (s *Scheduler) inOrgSnapshot(ctx context.Context, fn func(*Scheduler) error) ([]string, bool, error) {
	if s.inTransaction {
		return nil, false, nil
	}
	txStore, ok := s.store.(interface {
		InTransactionWithOptions(context.Context, pgx.TxOptions, func(*db.Queries) error) error
	})
	if !ok {
		return nil, false, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		var ids []string
		err := txStore.InTransactionWithOptions(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite}, func(q *db.Queries) error {
			txScheduler := NewScheduler(q, NewPolicyService(q), s.runner, WithSchedulerClock(s.clock))
			txScheduler.inTransaction = true
			txScheduler.deferredDispatch = &ids
			return fn(txScheduler)
		})
		if isSerializationFailure(err) {
			continue
		}
		return ids, true, err
	}
	return nil, true, fmt.Errorf("Dream scheduler transaction retry bound exceeded")
}

func (s *Scheduler) publishDeferred(ctx context.Context, ids []string, operation string) error {
	for _, id := range ids {
		if err := s.runner.Enqueue(ctx, JobTypeDream, id); err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
	}
	return nil
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func (s *Scheduler) explicitHierarchySnapshot(ctx context.Context, req BackfillRequest, p Policy, orgVersion int64) (Coverage, []MissingInput, error) {
	if !hasDreamSource(p.InputSources, sdkdream.SourceChildDreamSummary) {
		return Coverage{}, []MissingInput{}, nil
	}
	space, err := s.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{EnterpriseID: req.EnterpriseID, OrgScope: p.OrgUnitID})
	if err != nil {
		return Coverage{}, nil, err
	}
	if space.OrgVersion < 1 || space.OrgVersion > orgVersion {
		return Coverage{}, nil, fmt.Errorf("Dream parent exceeds pinned org version %d", orgVersion)
	}
	ref, ok := parseScopeRef(p.OrgUnitID)
	if !ok {
		return Coverage{}, nil, fmt.Errorf("invalid Dream hierarchy scope")
	}
	children, err := s.store.ListDreamImmediateChildren(ctx, db.ListDreamImmediateChildrenParams{EnterpriseID: req.EnterpriseID, ParentScopeKind: space.Kind, ParentScopeID: ref.id, ResultLimit: maxHierarchyChildren + 1})
	if err != nil {
		return Coverage{}, nil, err
	}
	if len(children) > int(maxHierarchyChildren) {
		return Coverage{}, nil, fmt.Errorf("Dream hierarchy children exceed bound %d", maxHierarchyChildren)
	}
	policyRows, err := s.store.ListPublishedDreamPolicies(ctx, req.EnterpriseID)
	if err != nil {
		return Coverage{}, nil, err
	}
	if len(policyRows) > maxHierarchyPolicies {
		return Coverage{}, nil, fmt.Errorf("published Dream policies exceed bound %d", maxHierarchyPolicies)
	}
	policies := make(map[string]struct {
		id  string
		max int32
	}, len(policyRows))
	for _, row := range policyRows {
		childPolicy, _, loadErr := s.policy.LoadPublished(ctx, row.ID)
		if loadErr != nil {
			return Coverage{}, nil, loadErr
		}
		policies[childPolicy.OrgUnitID] = struct {
			id  string
			max int32
		}{row.ID, childPolicy.MaxAttempts}
	}
	expected := make([]string, 0, len(children))
	states := make([]ChildRunState, 0, len(children))
	explicit := make([]MissingInput, 0)
	maxAttempts := make(map[string]int32, len(children))
	for _, child := range children {
		if child.OrgVersion > orgVersion {
			return Coverage{}, nil, fmt.Errorf("Dream child %s exceeds pinned org version %d", child.OrgScope, orgVersion)
		}
		if child.EnterpriseID != req.EnterpriseID || child.ParentOrgScope != p.OrgUnitID || child.OrgScope == "" {
			continue
		}
		expected = append(expected, child.OrgScope)
		childPolicy, exists := policies[child.OrgScope]
		if !exists {
			explicit = append(explicit, MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child.OrgScope, Reason: sdkdream.MissingNotFound})
			continue
		}
		maxAttempts[child.OrgScope] = childPolicy.max
		runs, listErr := s.listBoundedRuns(ctx, req.EnterpriseID, child.OrgScope)
		if listErr != nil {
			return Coverage{}, nil, listErr
		}
		for _, run := range runs {
			if run.PolicyID == childPolicy.id && run.WindowStart.Valid && run.WindowEnd.Valid && run.WindowStart.Time.Equal(req.WindowStart) && run.WindowEnd.Time.Equal(req.WindowEnd) {
				states = append(states, ChildRunState{OrgUnitID: child.OrgScope, Status: run.Status, Attempt: run.Attempt})
			}
		}
	}
	sort.Strings(expected)
	ready, coverage, missing, err := childReadiness(expected, states, explicit, p.AllowPartialChildren, maxAttempts)
	if err != nil {
		return Coverage{}, nil, err
	}
	if !ready {
		return Coverage{}, nil, fmt.Errorf("Dream explicit parent window is waiting for immediate children")
	}
	return coverage, missing, nil
}

func (s *Scheduler) listBoundedRuns(ctx context.Context, enterpriseID, orgUnitID string) ([]db.DreamRun, error) {
	runs, err := s.store.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{EnterpriseID: enterpriseID, OrgUnitID: orgUnitID, ResultLimit: int32(maxHierarchyRuns + 1)})
	if err != nil {
		return nil, err
	}
	if len(runs) > maxHierarchyRuns {
		return nil, fmt.Errorf("Dream hierarchy runs exceed bound %d", maxHierarchyRuns)
	}
	return runs, nil
}

func schedulerSnapshots(p Policy) ([]byte, []byte, error) {
	sourceCounts := make([]map[string]any, 0, len(p.InputSources))
	for _, source := range p.InputSources {
		sourceCounts = append(sourceCounts, map[string]any{"source_type": source, "count": 0})
	}
	input, err := json.Marshal(map[string]any{"source_counts": sourceCounts, "sanitized_input_ids": []string{}})
	if err != nil {
		return nil, nil, err
	}
	visibility, err := json.Marshal(map[string]any{"visibility_level": p.VisibilityLevel, "org_unit_ids": []string{p.OrgUnitID}, "masked_field_count": 0})
	return input, visibility, err
}

func validateExplicitIdempotency(existing db.DreamRun, req BackfillRequest, p Policy, version int32, operationKind string) (string, error) {
	rerunMatches := existing.RerunOfRunID.Valid == (req.RerunOfRunID != "") && (!existing.RerunOfRunID.Valid || existing.RerunOfRunID.String == req.RerunOfRunID)
	if existing.EnterpriseID != req.EnterpriseID || existing.PolicyID != req.PolicyID || existing.PolicyVersion != version || existing.Version != version || existing.OrgUnitID != p.OrgUnitID || existing.OperationKind != operationKind || !existing.WindowStart.Valid || !existing.WindowEnd.Valid || !existing.WindowStart.Time.Equal(req.WindowStart) || !existing.WindowEnd.Time.Equal(req.WindowEnd) || !rerunMatches || !existing.AuditRefID.Valid || existing.AuditRefID.String != req.AuditRefID {
		return "", fmt.Errorf("Dream idempotency key is already bound to a different canonical request")
	}
	return existing.ID, nil
}
