// Package worker owns the async plane: one tasks.Runner over the NATS
// JetStream bus with every async job type registered — artifact processing,
// index jobs, dream runs, and (Goal B5) workflow runs. atlas-api only
// enqueues; this worker consumes.
package worker

import (
	"context"
	"fmt"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// Registrar registers a job handler on the runner it was constructed with
// (artifacts.Service, dream.Runner satisfy it).
type Registrar interface {
	RegisterJobHandler() error
}

// RunnerRegistrar registers on an explicit runner (retrieval.Indexer).
type RunnerRegistrar interface {
	RegisterJobHandler(r *tasks.Runner) error
}

// Deps carries the async job registrations. Nil entries are skipped — the
// composition root decides which job types this worker instance serves.
type Deps struct {
	Artifacts Registrar       // artifact_processing
	Dreams    Registrar       // dream_run
	Indexer   RunnerRegistrar // index_job
	Extra     []Registrar     // additional job types (workflow runs, Goal B5)
}

// Worker wires the handlers and drives the runner lifecycle.
type Worker struct {
	runner *tasks.Runner
	deps   Deps
}

func New(runner *tasks.Runner, deps Deps) *Worker {
	return &Worker{runner: runner, deps: deps}
}

// Start registers every configured handler and subscribes the runner.
func (w *Worker) Start(ctx context.Context) error {
	if w.deps.Artifacts != nil {
		if err := w.deps.Artifacts.RegisterJobHandler(); err != nil {
			return fmt.Errorf("register artifact handler: %w", err)
		}
	}
	if w.deps.Dreams != nil {
		if err := w.deps.Dreams.RegisterJobHandler(); err != nil {
			return fmt.Errorf("register dream handler: %w", err)
		}
	}
	if w.deps.Indexer != nil {
		if err := w.deps.Indexer.RegisterJobHandler(w.runner); err != nil {
			return fmt.Errorf("register index handler: %w", err)
		}
	}
	for i, extra := range w.deps.Extra {
		if extra == nil {
			continue
		}
		if err := extra.RegisterJobHandler(); err != nil {
			return fmt.Errorf("register extra handler %d: %w", i, err)
		}
	}
	if err := w.runner.Start(ctx); err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	return nil
}

// Stop drains the subscriptions.
func (w *Worker) Stop() { w.runner.Stop() }
