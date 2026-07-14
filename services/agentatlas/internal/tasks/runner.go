// Package tasks is the Task Orchestrator v1: PostgreSQL owns job state and
// idempotency, a message bus (NATS JetStream in production, in-memory in unit
// tests) dispatches work. Artifact, index, and dream jobs share this runner.
package tasks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// Bus dispatches job ids by type.
type Bus interface {
	Publish(ctx context.Context, jobType, jobID string) error
	// Subscribe registers a handler for one job type. Returns an unsubscribe.
	Subscribe(ctx context.Context, jobType string, handler func(ctx context.Context, jobID string)) (func(), error)
}

// Handler owns one job type's lifecycle. Claim must transition
// pending->running atomically and return false when the job was already
// claimed (idempotent redelivery). Complete records the terminal state.
type Handler struct {
	Claim    func(ctx context.Context, jobID string) (bool, error)
	Execute  func(ctx context.Context, jobID string) error
	Complete func(ctx context.Context, jobID string, execErr error) error
}

type Runner struct {
	bus      Bus
	mu       sync.Mutex
	handlers map[string]Handler
	allowed  map[string]bool
	unsubs   []func()
}

func NewRunner(bus Bus) *Runner {
	return &Runner{bus: bus, handlers: map[string]Handler{}, allowed: map[string]bool{}}
}

// AllowEnqueue marks job types this runner may PRODUCE without consuming —
// the producer/consumer split: atlas-api enqueues, atlas-worker registers the
// handlers and consumes. Keeps Enqueue's typo guard for everything else.
func (r *Runner) AllowEnqueue(jobTypes ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range jobTypes {
		r.allowed[t] = true
	}
}

func (r *Runner) Register(jobType string, h Handler) error {
	if h.Claim == nil || h.Execute == nil || h.Complete == nil {
		return fmt.Errorf("tasks: handler for %q incomplete", jobType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[jobType]; dup {
		return fmt.Errorf("tasks: handler for %q registered twice", jobType)
	}
	r.handlers[jobType] = h
	return nil
}

func (r *Runner) Enqueue(ctx context.Context, jobType, jobID string) error {
	r.mu.Lock()
	_, registered := r.handlers[jobType]
	known := registered || r.allowed[jobType]
	r.mu.Unlock()
	if !known {
		return fmt.Errorf("tasks: enqueue for unknown job type %q", jobType)
	}
	return r.bus.Publish(ctx, jobType, jobID)
}

// Start subscribes every registered handler.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for jobType, h := range r.handlers {
		handler := h
		unsub, err := r.bus.Subscribe(ctx, jobType, func(ctx context.Context, jobID string) {
			r.process(ctx, handler, jobID)
		})
		if err != nil {
			return fmt.Errorf("tasks: subscribe %s: %w", jobType, err)
		}
		r.unsubs = append(r.unsubs, unsub)
	}
	return nil
}

func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.unsubs {
		u()
	}
	r.unsubs = nil
}

// JobWorkCaseAdvance is the job type whose handler drives the Task 0E
// workcase.Orchestrator.Advance(ctx, caseID) exactly once per delivery; the
// jobID IS the caseID. Advance is idempotent and restart-safe, so duplicate or
// out-of-order JetStream deliveries converge without a double side effect,
// while Handler.Claim still gives at-most-one in-flight execution per delivery.
const JobWorkCaseAdvance = "workcase.advance"

// WorkCaseAdvanceHandler builds the Runner Handler for JobWorkCaseAdvance from
// an idempotent claim/complete pair and the orchestrator's advance entry point.
// Package tasks deliberately does NOT import package workcase (that would be an
// import cycle: the orchestrator enqueues advance jobs through this runner), so
// the advance function is taken as a plain func(ctx, caseID) error.
func WorkCaseAdvanceHandler(
	claim func(ctx context.Context, caseID string) (bool, error),
	advance func(ctx context.Context, caseID string) error,
	complete func(ctx context.Context, caseID string, execErr error) error,
) Handler {
	return Handler{Claim: claim, Execute: advance, Complete: complete}
}

// Process runs one job synchronously (used by tests and inline callers).
func (r *Runner) Process(ctx context.Context, jobType, jobID string) error {
	r.mu.Lock()
	h, ok := r.handlers[jobType]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("tasks: no handler for %q", jobType)
	}
	return r.process(ctx, h, jobID)
}

func (r *Runner) process(ctx context.Context, h Handler, jobID string) error {
	claimed, err := h.Claim(ctx, jobID)
	if err != nil {
		return fmt.Errorf("tasks: claim %s: %w", jobID, err)
	}
	if !claimed {
		return nil // already processed or in flight — idempotent redelivery
	}
	execErr := h.Execute(ctx, jobID)
	if cerr := h.Complete(ctx, jobID, execErr); cerr != nil {
		return fmt.Errorf("tasks: complete %s: %w", jobID, cerr)
	}
	return execErr
}

// --- in-memory bus (unit tests, single-process runs) ------------------------

type MemBus struct {
	mu   sync.Mutex
	subs map[string][]func(ctx context.Context, jobID string)
}

func NewMemBus() *MemBus {
	return &MemBus{subs: map[string][]func(ctx context.Context, jobID string){}}
}

func (b *MemBus) Publish(ctx context.Context, jobType, jobID string) error {
	b.mu.Lock()
	handlers := append([]func(ctx context.Context, jobID string){}, b.subs[jobType]...)
	b.mu.Unlock()
	for _, h := range handlers {
		h(ctx, jobID)
	}
	return nil
}

func (b *MemBus) Subscribe(_ context.Context, jobType string, handler func(ctx context.Context, jobID string)) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[jobType] = append(b.subs[jobType], handler)
	idx := len(b.subs[jobType]) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.subs[jobType][idx] = func(context.Context, string) {}
	}, nil
}

// --- NATS JetStream bus ------------------------------------------------------

const streamName = "AGENTATLAS_JOBS"

// NATSBus dispatches through a JetStream stream (durable consumers per job
// type) so restarts redeliver unacked work; PG claims keep it idempotent.
type NATSBus struct {
	js jetstream.JetStream
}

// NewNATSBus connects to NATS JetStream. tlsMgr configures the NATS link's
// transport security (services/agentatlas/internal/transportsecurity);
// nil, or a Manager built with LinkConfig.Mode == ModeOff, keeps today's
// plaintext (nats://) behavior unchanged.
func NewNATSBus(ctx context.Context, url string, tlsMgr *transportsecurity.Manager) (*NATSBus, error) {
	opts := []nats.Option{nats.Timeout(10 * time.Second)}
	if tlsMgr != nil {
		tlsCfg, err := tlsMgr.ClientTLSConfigOrNil()
		if err != nil {
			return nil, fmt.Errorf("nats tls: %w", err)
		}
		if tlsCfg != nil {
			opts = append(opts, nats.Secure(tlsCfg))
		}
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"agentatlas.jobs.>"},
		Storage:  jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("jetstream stream: %w", err)
	}
	return &NATSBus{js: js}, nil
}

func subjectFor(jobType string) string {
	return "agentatlas.jobs." + strings.ReplaceAll(jobType, ".", "_")
}

func (b *NATSBus) Publish(ctx context.Context, jobType, jobID string) error {
	_, err := b.js.Publish(ctx, subjectFor(jobType), []byte(jobID))
	return err
}

func (b *NATSBus) Subscribe(ctx context.Context, jobType string, handler func(ctx context.Context, jobID string)) (func(), error) {
	consumer, err := b.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       "worker_" + strings.ReplaceAll(jobType, ".", "_"),
		FilterSubject: subjectFor(jobType),
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    5,
	})
	if err != nil {
		return nil, err
	}
	cc, err := consumer.Consume(func(msg jetstream.Msg) {
		handler(ctx, string(msg.Data()))
		_ = msg.Ack()
	})
	if err != nil {
		return nil, err
	}
	return cc.Stop, nil
}
