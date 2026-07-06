package dream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

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
	CreateDreamRun(ctx context.Context, arg db.CreateDreamRunParams) (db.DreamRun, error)
	GetLatestDreamRunForPolicy(ctx context.Context, policyID string) (db.DreamRun, error)
}

// Scheduler turns published policies into due dream runs. Run ids are
// deterministic per (policy, window) so double ticks stay idempotent.
type Scheduler struct {
	store  SchedulerStore
	policy *PolicyService
	runner *tasks.Runner
}

func NewScheduler(store SchedulerStore, policy *PolicyService, runner *tasks.Runner) *Scheduler {
	return &Scheduler{store: store, policy: policy, runner: runner}
}

// runIDFor is deterministic: same policy+window => same run id (PK dedupes).
func runIDFor(policyID string, windowEnd time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", policyID, windowEnd.Unix())))
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
	// find the latest firing <= now by stepping from a reference point
	ref := lastWindowEnd
	if ref.IsZero() {
		ref = now.Add(-48 * time.Hour)
	}
	var lastFire time.Time
	for t := sched.Next(ref); !t.After(now); t = sched.Next(t) {
		lastFire = t
	}
	if lastFire.IsZero() {
		return time.Time{}, time.Time{}, false, nil // next firing is in the future
	}
	start = lastWindowEnd
	if start.IsZero() {
		start = lastFire.Add(-24 * time.Hour)
	}
	return start, lastFire, true, nil
}

// Tick scans published policies of one enterprise and dispatches due runs.
func (s *Scheduler) Tick(ctx context.Context, enterpriseID string, now time.Time) (int, error) {
	policies, err := s.store.ListPublishedDreamPolicies(ctx, enterpriseID)
	if err != nil {
		return 0, fmt.Errorf("list policies: %w", err)
	}
	dispatched := 0
	for _, row := range policies {
		p, version, err := s.policy.LoadPublished(ctx, row.ID)
		if err != nil {
			return dispatched, err
		}
		var lastEnd time.Time
		last, err := s.store.GetLatestDreamRunForPolicy(ctx, row.ID)
		switch {
		case err == nil:
			lastEnd = last.WindowEnd.Time
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return dispatched, err
		}
		start, end, due, err := Due(p, lastEnd, now)
		if err != nil {
			return dispatched, err
		}
		if !due {
			continue
		}
		runID := runIDFor(row.ID, end)
		if _, err := s.store.CreateDreamRun(ctx, db.CreateDreamRunParams{
			ID: runID, PolicyID: row.ID, Version: version,
			EnterpriseID: enterpriseID, Status: "pending",
			WindowStart: pgtype.Timestamptz{Time: start, Valid: true},
			WindowEnd:   pgtype.Timestamptz{Time: end, Valid: true},
		}); err != nil {
			// Only a duplicate PK means another scheduler already created this
			// window's run; any other error is a real failure — fail loud, do
			// not report the window as dispatched.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				continue
			}
			return dispatched, fmt.Errorf("create dream run %s: %w", runID, err)
		}
		if err := s.runner.Enqueue(ctx, JobTypeDream, runID); err != nil {
			return dispatched, fmt.Errorf("dispatch dream run: %w", err)
		}
		dispatched++
	}
	return dispatched, nil
}
