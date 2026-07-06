package worker

import (
	"context"
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// fakeRegistrar registers a counting handler for one job type — the worker's
// wiring/lifecycle is under test here; the real handlers' behavior is covered
// by their own package tests.
type fakeRegistrar struct {
	runner   *tasks.Runner
	jobType  string
	executed map[string]int
}

func (f *fakeRegistrar) RegisterJobHandler() error {
	return f.runner.Register(f.jobType, tasks.Handler{
		Claim:   func(context.Context, string) (bool, error) { return true, nil },
		Execute: func(_ context.Context, id string) error { f.executed[id]++; return nil },
		Complete: func(context.Context, string, error) error {
			return nil
		},
	})
}

type fakeIndexRegistrar struct {
	jobType  string
	executed map[string]int
}

func (f *fakeIndexRegistrar) RegisterJobHandler(r *tasks.Runner) error {
	return r.Register(f.jobType, tasks.Handler{
		Claim:   func(context.Context, string) (bool, error) { return true, nil },
		Execute: func(_ context.Context, id string) error { f.executed[id]++; return nil },
		Complete: func(context.Context, string, error) error {
			return nil
		},
	})
}

func TestWorkerRegistersAndConsumesEveryJobType(t *testing.T) {
	ctx := context.Background()
	runner := tasks.NewRunner(tasks.NewMemBus())
	executed := map[string]int{}

	w := New(runner, Deps{
		Artifacts: &fakeRegistrar{runner: runner, jobType: artifacts.JobTypeArtifact, executed: executed},
		Dreams:    &fakeRegistrar{runner: runner, jobType: dream.JobTypeDream, executed: executed},
		Indexer:   &fakeIndexRegistrar{jobType: retrieval.JobTypeIndex, executed: executed},
	})
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// One job of each async type flows through the shared runner (MemBus
	// delivers synchronously).
	for jobType, id := range map[string]string{
		artifacts.JobTypeArtifact: "job_art",
		retrieval.JobTypeIndex:    "job_idx",
		dream.JobTypeDream:        "job_dream",
	} {
		if err := runner.Enqueue(ctx, jobType, id); err != nil {
			t.Fatalf("enqueue %s: %v", jobType, err)
		}
	}
	for _, id := range []string{"job_art", "job_idx", "job_dream"} {
		if executed[id] != 1 {
			t.Fatalf("job %s executed %d times, want 1 (executed=%v)", id, executed[id], executed)
		}
	}

	// Stop drains: post-drain publishes are not handled.
	w.Stop()
	if err := runner.Enqueue(ctx, dream.JobTypeDream, "job_after_stop"); err != nil {
		t.Fatalf("enqueue after stop: %v", err)
	}
	if executed["job_after_stop"] != 0 {
		t.Fatal("job executed after Stop — drain broken")
	}
}

func TestProducerOnlyRunnerEnqueuesAllowedTypes(t *testing.T) {
	// atlas-api pattern: no handlers, only AllowEnqueue.
	producer := tasks.NewRunner(tasks.NewMemBus())
	producer.AllowEnqueue(retrieval.JobTypeIndex)
	if err := producer.Enqueue(context.Background(), retrieval.JobTypeIndex, "j1"); err != nil {
		t.Fatalf("allowed enqueue: %v", err)
	}
	if err := producer.Enqueue(context.Background(), "ghost_type", "j2"); err == nil {
		t.Fatal("unknown type must still fail loud")
	}
}
