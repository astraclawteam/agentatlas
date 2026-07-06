package tasks

import (
	"context"
	"errors"
	"testing"
)

type jobState struct {
	status   string
	executed int
	lastErr  error
}

func handlerFor(states map[string]*jobState, execErr error) Handler {
	return Handler{
		Claim: func(_ context.Context, id string) (bool, error) {
			st := states[id]
			if st.status != "pending" {
				return false, nil
			}
			st.status = "running"
			return true, nil
		},
		Execute: func(_ context.Context, id string) error {
			states[id].executed++
			return execErr
		},
		Complete: func(_ context.Context, id string, execErr error) error {
			st := states[id]
			st.lastErr = execErr
			if execErr != nil {
				st.status = "failed"
			} else {
				st.status = "succeeded"
			}
			return nil
		},
	}
}

func TestRunnerExecutesOnce(t *testing.T) {
	states := map[string]*jobState{"j1": {status: "pending"}}
	bus := NewMemBus()
	r := NewRunner(bus)
	if err := r.Register("demo", handlerFor(states, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := r.Enqueue(context.Background(), "demo", "j1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if states["j1"].executed != 1 || states["j1"].status != "succeeded" {
		t.Fatalf("state = %+v", states["j1"])
	}

	// Redelivery must be idempotent: claim fails, no second execution.
	if err := bus.Publish(context.Background(), "demo", "j1"); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if states["j1"].executed != 1 {
		t.Fatalf("redelivery executed twice: %+v", states["j1"])
	}
}

func TestRunnerRecordsFailure(t *testing.T) {
	states := map[string]*jobState{"j2": {status: "pending"}}
	r := NewRunner(NewMemBus())
	boom := errors.New("parse exploded")
	_ = r.Register("demo", handlerFor(states, boom))
	err := r.Process(context.Background(), "demo", "j2")
	if !errors.Is(err, boom) {
		t.Fatalf("Process must surface the exec error, got %v", err)
	}
	if states["j2"].status != "failed" || !errors.Is(states["j2"].lastErr, boom) {
		t.Fatalf("state = %+v", states["j2"])
	}
}

func TestRunnerRejectsUnknownType(t *testing.T) {
	r := NewRunner(NewMemBus())
	if err := r.Enqueue(context.Background(), "ghost", "j"); err == nil {
		t.Fatal("unknown job type must fail")
	}
	if err := r.Register("half", Handler{}); err == nil {
		t.Fatal("incomplete handler must fail")
	}
}
