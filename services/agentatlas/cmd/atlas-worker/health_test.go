package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// The observed run: atlas-worker reported `healthy` continuously while its
// org-version cursor subscription to AgentNexus failed 401 forever -- eight
// failures in ninety seconds, on a fixed ~5s interval, with no backoff and no
// give-up. Its healthcheck was the /metrics listener, which is pure liveness,
// and a worker whose core subscription can never succeed serves /metrics
// perfectly.
//
// These assertions go over a real HTTP round trip against the composed mux.

func workerHealth(t *testing.T, mux http.Handler, path string) (int, bool, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	var body struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s body %q: %v", path, recorder.Body.String(), err)
	}
	return recorder.Code, body.Ready, body.Reason
}

// Eight failures in ninety seconds is what actually happened. It must not
// answer 200.
func TestAPermanentlyFailingOrgCursorReachesReadiness(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	for i := 0; i < 8; i++ {
		cursors.Failed("ent_1")
	}
	mux := newWorkerHealthMux(observability.NewMetrics(), cursors)

	status, ready, reason := workerHealth(t, mux, "/readyz")
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v after 8 consecutive subscription failures", status, ready)
	}
	if !strings.Contains(reason, "8") {
		t.Errorf("reason %q does not say how long this has been failing", reason)
	}

	// Liveness stays up: the process is fine, its subscription is not, and a
	// flapping container would hide the reason.
	if status, _, _ := workerHealth(t, mux, "/healthz"); status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 - the process must stay observable", status)
	}

	// /metrics keeps working. It is the thing that was green throughout, so a
	// fix that broke it would be trading one blind spot for another.
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", recorder.Code)
	}
}

// A worker whose subscriptions work must still be ready, or the assertion above
// would pass against a surface that reports 503 unconditionally.
func TestAWorkingOrgCursorIsReady(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	cursors.Succeeded("ent_1")
	status, ready, reason := workerHealth(t, newWorkerHealthMux(observability.NewMetrics(), cursors), "/readyz")
	if status != http.StatusOK || !ready {
		t.Fatalf("/readyz status=%d ready=%v reason=%q with a working subscription", status, ready, reason)
	}
}

// A worker that subscribes to nothing (ATLAS_ORG_SYNC_ENTERPRISES unset) is
// ready. Reporting a deployment that opted out as broken would train operators
// to ignore this endpoint.
func TestAWorkerWithNoSubscriptionsIsReady(t *testing.T) {
	status, ready, _ := workerHealth(t, newWorkerHealthMux(observability.NewMetrics(), newOrgCursorHealth(orgCursorFailureThreshold)), "/readyz")
	if status != http.StatusOK || !ready {
		t.Fatalf("/readyz status=%d ready=%v with no subscriptions configured", status, ready)
	}
}

// A brief blip must not page anyone: readiness degrades on a failure RUN, not
// on a single failed attempt, and a stream that reconnects clears it.
func TestABlipDoesNotUnreadyTheWorkerAndRecoveryClearsIt(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	cursors.Failed("ent_1")
	if status, ready, _ := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz"); status != http.StatusOK || !ready {
		t.Fatalf("one failed attempt made the worker unready: status=%d ready=%v", status, ready)
	}

	for i := 0; i < orgCursorFailureThreshold; i++ {
		cursors.Failed("ent_1")
	}
	if status, _, _ := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("a sustained failure run left the worker ready: status=%d", status)
	}

	cursors.Succeeded("ent_1")
	if status, ready, _ := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz"); status != http.StatusOK || !ready {
		t.Fatalf("a recovered subscription stayed unready: status=%d ready=%v", status, ready)
	}
}

// ErrOrgCursorAhead makes the loop return: that tenant is unsynced for the life
// of the process. It must never clear, and one failed attempt's worth of
// tolerance does not apply to it.
func TestAnAbandonedSubscriptionIsImmediatelyAndPermanentlyUnready(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	cursors.Abandoned("ent_1")
	status, ready, reason := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz")
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v after a subscription stopped for good", status, ready)
	}
	if !strings.Contains(reason, "permanently") {
		t.Errorf("reason %q does not distinguish a permanent stop from a retry loop", reason)
	}

	// A later success on some other tenant cannot un-abandon this one.
	cursors.Succeeded("ent_1")
	if status, _, _ := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("an abandoned subscription cleared itself: status=%d", status)
	}
}

// --- the link between the retry loop and the health surface -----------------
//
// The assertions above drive orgCursorHealth directly, so on their own they
// would stay green against a build whose retry loop had quietly stopped
// reporting. These drive the REAL loop against a subscription that fails the
// way the observed one did -- 401, forever, on every attempt -- and ask the
// real mux what it answers.

func TestTheRetryLoopReportsAPermanentFailureToTheHealthSurface(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOrgCursor(ctx, orgCursor{
			Run: func(context.Context) error {
				select {
				case attempts <- struct{}{}:
				default:
				}
				// What AgentNexus actually returned, over and over.
				return errors.New("nexus /v1/org-events: status 401")
			},
			EnterpriseID: "ent_1",
			Health:       cursors,
			Logger:       zap.NewNop(),
			RetryDelay:   time.Millisecond,
		})
	}()

	// Wait for a failure run long enough to matter, then read the surface.
	for i := 0; i < orgCursorFailureThreshold+1; i++ {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("the subscription loop stopped attempting")
		}
	}
	// The loop records the failure after Run returns, so allow the last
	// attempt to complete its bookkeeping.
	deadline := time.Now().Add(5 * time.Second)
	var status int
	var ready bool
	var reason string
	for time.Now().Before(deadline) {
		status, ready, reason = workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz")
		if status == http.StatusServiceUnavailable {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("a subscription failing 401 on every attempt left /readyz at status=%d ready=%v reason=%q", status, ready, reason)
	}
	cancel()
	<-done
}

// The give-up path: ErrOrgCursorAhead ends the loop, and that tenant is
// unsynced for the life of the process. The surface must say so, and the loop
// must actually stop rather than spin.
func TestTheRetryLoopReportsGivingUp(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	calls := 0
	runOrgCursor(context.Background(), orgCursor{
		Run:          func(context.Context) error { calls++; return nexusclient.ErrOrgCursorAhead },
		EnterpriseID: "ent_1",
		Health:       cursors,
		Logger:       zap.NewNop(),
		RetryDelay:   time.Millisecond,
	})
	if calls != 1 {
		t.Errorf("the loop retried an error that can never succeed: %d attempts", calls)
	}
	if status, ready, _ := workerHealth(t, newWorkerHealthMux(nil, cursors), "/readyz"); status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v after the subscription gave up", status, ready)
	}
}

// The readiness body is served unauthenticated on the metrics port. Which
// enterprises a deployment syncs is customer information; the counts are not.
func TestReadinessReasonNamesNoTenant(t *testing.T) {
	cursors := newOrgCursorHealth(orgCursorFailureThreshold)
	for i := 0; i < 5; i++ {
		cursors.Failed("ent_acme_manufacturing")
	}
	cursors.Abandoned("ent_globex")
	recorder := httptest.NewRecorder()
	newWorkerHealthMux(nil, cursors).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	for _, tenant := range []string{"ent_acme_manufacturing", "ent_globex"} {
		if strings.Contains(recorder.Body.String(), tenant) {
			t.Fatalf("the readiness body names a tenant: %s", recorder.Body.String())
		}
	}
}
