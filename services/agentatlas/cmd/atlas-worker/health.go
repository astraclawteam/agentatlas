package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// orgCursorFailureThreshold is how many consecutive failed attempts at one
// tenant's org-version stream make this worker unready.
//
// The retry loop runs on a fixed ~5s interval, so three attempts is roughly
// fifteen seconds of a subscription that is not working. That is long enough to
// ride out a restart of the peer and far short of the ninety seconds over which
// this failure went unreported.
const orgCursorFailureThreshold = 3

// orgCursorHealth is the health surface for the worker's AgentNexus org-version
// subscriptions.
//
// atlas-worker reported healthy continuously while its org-version cursor
// subscription failed 401 forever: eight failures in ninety seconds, on a fixed
// interval, with no backoff and no give-up. Nothing was wrong with the
// container, so the healthcheck -- the /metrics listener, which is pure
// liveness -- stayed green the whole time.
//
// A core subscription that cannot succeed must reach the service's health
// surface. Retrying silently behind a green check is exactly how this stayed
// invisible for the length of a deployment.
//
// It deliberately holds counts and not tenant identifiers in anything it
// publishes: the readiness body goes out unauthenticated on the metrics port,
// and which enterprises this deployment syncs is customer information. The
// enterprise id and the underlying error go to the logger, which is where an
// operator with access to the host looks next.
type orgCursorHealth struct {
	mu        sync.Mutex
	failures  map[string]int
	abandoned map[string]bool
	threshold int
}

func newOrgCursorHealth(threshold int) *orgCursorHealth {
	return &orgCursorHealth{
		failures:  map[string]int{},
		abandoned: map[string]bool{},
		threshold: threshold,
	}
}

// Failed records one failed attempt at a tenant's stream.
func (h *orgCursorHealth) Failed(enterpriseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[enterpriseID]++
}

// Succeeded clears a tenant's failure run. A stream that ran and ended cleanly
// is a working subscription, however briefly it lasted.
func (h *orgCursorHealth) Succeeded(enterpriseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[enterpriseID] = 0
}

// Abandoned records that a tenant's subscription stopped for good. The cursor
// loop gives up on ErrOrgCursorAhead because retrying can never succeed; that
// tenant is then permanently unsynced, which is a stronger statement than a
// failure run and never clears on its own.
func (h *orgCursorHealth) Abandoned(enterpriseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.abandoned[enterpriseID] = true
}

// NotReadyReason states why the worker is not ready, or "" when every
// subscription is working. It counts tenants rather than naming them.
func (h *orgCursorHealth) NotReadyReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if abandoned := len(h.abandoned); abandoned > 0 {
		return fmt.Sprintf("%d AgentNexus org-version subscription(s) stopped permanently; this worker no longer tracks their sealed org version", abandoned)
	}
	stuck, worst := 0, 0
	for _, count := range h.failures {
		if count >= h.threshold {
			stuck++
		}
		if count > worst {
			worst = count
		}
	}
	if stuck == 0 {
		return ""
	}
	return fmt.Sprintf("%d AgentNexus org-version subscription(s) have failed every attempt; the longest run is %d consecutive failures", stuck, worst)
}

// orgCursorRetryDelay paces one tenant's subscription retries. It is the
// interval the observed run measured -- eight attempts in ninety seconds --
// kept as-is: adding backoff is a separate, lower-priority change, and doing it
// here would have changed the failure this work is about reporting.
const orgCursorRetryDelay = 5 * time.Second

// orgCursor is one tenant's AgentNexus org-version subscription loop.
type orgCursor struct {
	// Run subscribes and returns when the stream ends. It is a func rather than
	// the syncer itself so the loop below can be driven against a failure that
	// never recovers, which is the case that shipped.
	Run          func(context.Context) error
	EnterpriseID string
	Health       *orgCursorHealth
	Logger       *zap.Logger
	RetryDelay   time.Duration
}

// runOrgCursor keeps one tenant's org-version subscription alive, and reports
// what it is doing to the health surface.
//
// It is a function rather than an inline goroutine in run() because the link it
// owns -- a failing subscription becoming a 503 -- is the whole point of this
// change, and the only other way to cover that link would be to grep main.go
// for a call, which proves a string is present and nothing about what the
// process does when the stream fails.
func runOrgCursor(ctx context.Context, cursor orgCursor) {
	for ctx.Err() == nil {
		err := cursor.Run(ctx)
		switch {
		case errors.Is(err, nexusclient.ErrOrgCursorAhead):
			// A cursor ahead of the sealed version can never succeed on retry;
			// looping would hammer the feed forever. Stop this tenant and say so.
			cursor.Logger.Error("org cursor is ahead of the sealed org version; not retrying",
				zap.String("enterprise_id", cursor.EnterpriseID), zap.Error(err))
			cursor.Health.Abandoned(cursor.EnterpriseID)
			return
		case err != nil && ctx.Err() == nil:
			// This is what used to happen forever behind a green healthcheck.
			// The log line is unchanged; what is new is that the failure now
			// reaches /readyz.
			cursor.Health.Failed(cursor.EnterpriseID)
			cursor.Logger.Warn("org version stream ended; retrying",
				zap.String("enterprise_id", cursor.EnterpriseID), zap.Error(err))
		case err == nil:
			cursor.Health.Succeeded(cursor.EnterpriseID)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cursor.RetryDelay):
		}
	}
}

// newWorkerHealthMux builds the listener atlas-worker exposes on
// ATLAS_WORKER_METRICS_ADDR.
//
// /metrics is unchanged. /healthz is liveness and always answers 200, so the
// container does not flap and the surface stays observable. /readyz is the one
// that reports whether this worker is actually doing its job, which is what the
// compose healthcheck now asks: before this, that healthcheck fetched /metrics,
// and a process with a permanently 401ing subscription serves /metrics
// perfectly.
//
// It is split out from run() so the contract can be exercised over a real HTTP
// round trip rather than asserted by reading main.go.
func newWorkerHealthMux(metrics *observability.Metrics, cursors *orgCursorHealth) *http.ServeMux {
	mux := http.NewServeMux()
	if metrics != nil {
		mux.Handle("/metrics", metrics.Handler())
	}
	status := func() app.HealthStatus {
		reason := cursors.NotReadyReason()
		return app.NewHealthStatus("atlas-worker", "agentnexus-org-cursor").MarkReady(reason == "")
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeWorkerHealth(w, http.StatusOK, status(), "")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		reason := cursors.NotReadyReason()
		health := app.NewHealthStatus("atlas-worker", "agentnexus-org-cursor").MarkReady(reason == "")
		code := http.StatusOK
		// Branch on the value the body carries rather than re-deriving the
		// predicate, so the status an orchestrator sees and the body an
		// operator reads can never disagree.
		if !health.Ready {
			code = http.StatusServiceUnavailable
		}
		writeWorkerHealth(w, code, health, reason)
	})
	return mux
}

func writeWorkerHealth(w http.ResponseWriter, code int, health app.HealthStatus, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The reason is an operator diagnostic about this worker's own
	// subscriptions; it carries counts, never a tenant identifier.
	if reason == "" {
		fmt.Fprintf(w, `{"service":%q,"version":%q,"ready":%t,"dependencies":["agentnexus-org-cursor"]}`,
			health.Service, health.Version, health.Ready)
		return
	}
	fmt.Fprintf(w, `{"service":%q,"version":%q,"ready":%t,"dependencies":["agentnexus-org-cursor"],"reason":%q}`,
		health.Service, health.Version, health.Ready, reason)
}
