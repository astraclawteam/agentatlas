package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// atlas-outcome-projector served NO HTTP listener whatsoever -- no /healthz, no
// /readyz, no metrics port -- while every other long-lived service in both
// products has at least one. Its runtime image is alpine with a single static
// binary and no psql, so nothing inside the container could answer "is the
// projection advancing?" either. It degrades silently by design: ProjectAll
// failures are logged at WARN and the watermark simply stops advancing.
//
// A probe that only reports "the process exists" would have been green through
// all of that. These assertions are about the lag, and about readiness tracking
// the outcome of a real projection pass.

func projectorHealth(t *testing.T, health *projectionHealth, path string) (int, bool, string, string) {
	t.Helper()
	mux, err := newProjectorHealthMux(health)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if path == "/metrics" {
		return recorder.Code, false, "", recorder.Body.String()
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s body %q: %v", path, recorder.Body.String(), err)
	}
	return recorder.Code, body.Ready, body.Reason, recorder.Body.String()
}

// A projector with a reachable graph and a real pass behind it is ready, and
// publishes a lag an operator can watch.
func TestAnAdvancingProjectionIsReadyAndPublishesItsLag(t *testing.T) {
	graph, outbox := outcomegraph.NewMemoryGraph(), outcomegraph.NewMemOutbox()
	appendEvents(t, outbox, "ent_1", 3)
	projector := outcomegraph.NewProjector(graph, outbox)
	health := newProjectionHealth()

	if err := observeProjection(context.Background(), projector, health); err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	status, ready, reason, _ := projectorHealth(t, health, "/readyz")
	if status != http.StatusOK || !ready {
		t.Fatalf("/readyz status=%d ready=%v reason=%q after a successful pass", status, ready, reason)
	}
	_, _, _, metrics := projectorHealth(t, health, "/metrics")
	for _, want := range []string{
		`agentatlas_outcome_projection_lag_events{tenant="ent_1"} 0`,
		`agentatlas_outcome_projection_watermark{tenant="ent_1"} 3`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics does not publish %s:\n%s", want, metrics)
		}
	}
}

// The failure that shipped: AGE goes away, ProjectAll fails, the watermark
// stops advancing, and the process keeps running. Readiness must follow the
// outcome of the pass, and the last known lag must survive on /metrics -- a
// series that vanishes when the graph does reads as "nothing to see".
func TestAStalledProjectionIsNotReadyAndItsLagIsVisible(t *testing.T) {
	graph, outbox := outcomegraph.NewMemoryGraph(), outcomegraph.NewMemOutbox()
	appendEvents(t, outbox, "ent_1", 2)
	projector := outcomegraph.NewProjector(graph, outbox)
	health := newProjectionHealth()
	if err := observeProjection(context.Background(), projector, health); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// AGE becomes unreachable, and four more committed facts arrive.
	graph.SetUnavailable(true)
	appendEvents(t, outbox, "ent_1", 4)
	if err := observeProjection(context.Background(), projector, health); err == nil {
		t.Fatal("the fixture's projection pass succeeded, so this asserts nothing")
	}

	status, ready, reason, raw := projectorHealth(t, health, "/readyz")
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v with a stalled projection: %s", status, ready, raw)
	}
	if !strings.Contains(reason, "watermark is not advancing") {
		t.Errorf("reason %q does not say the projection stalled", reason)
	}

	// Liveness stays up so the container does not flap and the surface stays
	// readable.
	if status, _, _, _ := projectorHealth(t, health, "/healthz"); status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 - the process must stay observable", status)
	}

	// The last watermark this projector actually reached is still published.
	_, _, _, metrics := projectorHealth(t, health, "/metrics")
	if !strings.Contains(metrics, `agentatlas_outcome_projection_watermark{tenant="ent_1"} 2`) {
		t.Errorf("the last known watermark vanished when the graph did:\n%s", metrics)
	}
}

// The lag is the deliverable, so it has to be a real number. An unprojected
// backlog must show up as that backlog -- a gauge that is always zero is the
// same false green as no gauge at all, and reads worse because it looks
// checked.
func TestAnUnprojectedBacklogIsPublishedAsLag(t *testing.T) {
	graph, outbox := outcomegraph.NewMemoryGraph(), outcomegraph.NewMemOutbox()
	appendEvents(t, outbox, "ent_1", 5)
	projector := outcomegraph.NewProjector(graph, outbox)

	lag, err := projector.Lag(context.Background())
	if err != nil {
		t.Fatalf("read lag: %v", err)
	}
	if len(lag) != 1 || lag[0].Events != 5 || lag[0].Watermark != 0 || lag[0].Head != 5 {
		t.Fatalf("lag = %+v, want 5 events behind a head of 5 with nothing projected", lag)
	}

	health := newProjectionHealth()
	health.CycleCompleted(nil)
	health.LagObserved(lag, nil)
	_, _, _, metrics := projectorHealth(t, health, "/metrics")
	for _, want := range []string{
		`agentatlas_outcome_projection_lag_events{tenant="ent_1"} 5`,
		`agentatlas_outcome_projection_outbox_head{tenant="ent_1"} 5`,
		`agentatlas_outcome_projection_watermark{tenant="ent_1"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics does not publish %s:\n%s", want, metrics)
		}
	}

	// And it falls to zero once the backlog is applied, so the number tracks
	// the projection rather than just counting the outbox.
	if err := observeProjection(context.Background(), projector, health); err != nil {
		t.Fatal(err)
	}
	if _, _, _, metrics := projectorHealth(t, health, "/metrics"); !strings.Contains(metrics, `agentatlas_outcome_projection_lag_events{tenant="ent_1"} 0`) {
		t.Errorf("the lag did not fall to zero after the backlog was projected:\n%s", metrics)
	}
}

// A lag that cannot even be READ is worse than a big one, and must not be
// reported as a healthy zero.
func TestAnUnreadableLagIsNotReportedAsZero(t *testing.T) {
	graph, outbox := outcomegraph.NewMemoryGraph(), outcomegraph.NewMemOutbox()
	appendEvents(t, outbox, "ent_1", 2)
	projector := outcomegraph.NewProjector(graph, outbox)
	health := newProjectionHealth()
	if err := observeProjection(context.Background(), projector, health); err != nil {
		t.Fatal(err)
	}
	graph.SetUnavailable(true)
	health.CycleCompleted(nil) // a pass that "succeeded" over an empty tenant set
	health.LagObserved(projector.Lag(context.Background()))

	status, ready, reason, raw := projectorHealth(t, health, "/readyz")
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v when the lag cannot be read: %s", status, ready, raw)
	}
	if !strings.Contains(reason, "unknown") {
		t.Errorf("reason %q does not say the lag is unknown", reason)
	}
}

// A process that has started but never completed a pass has proven nothing. It
// must not report itself ready simply because it is up -- that is precisely the
// "the process exists" probe this replaces.
func TestAProjectorThatHasNeverCompletedAPassIsNotReady(t *testing.T) {
	status, ready, reason, _ := projectorHealth(t, newProjectionHealth(), "/readyz")
	if status != http.StatusServiceUnavailable || ready {
		t.Fatalf("/readyz status=%d ready=%v before any projection pass ran", status, ready)
	}
	if !strings.Contains(reason, "no projection pass") {
		t.Errorf("reason %q does not say no pass has completed", reason)
	}
}

// The listener is unauthenticated inside the deployment network, and the graph
// store's own errors name its DSN.
func TestReadinessReasonCarriesNoConnectionString(t *testing.T) {
	health := newProjectionHealth()
	health.CycleCompleted(errAGEWithDSN)
	_, _, _, raw := projectorHealth(t, health, "/readyz")
	if strings.Contains(raw, "hunter2") || strings.Contains(raw, "age.internal") {
		t.Fatalf("the readiness body published a connection string: %s", raw)
	}
}

var errAGEWithDSN = &dsnError{}

type dsnError struct{}

func (*dsnError) Error() string {
	return "apache age: dial postgres://atlas:hunter2@age.internal:5432/outcome_graph: connection refused"
}

func appendEvents(t *testing.T, outbox *outcomegraph.MemOutbox, tenant string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		_, err := outbox.Append(outcomegraph.ProjectionEvent{
			Tenant:       tenant,
			Source:       outcomegraph.SourceWorkCase,
			Kind:         outcomegraph.KindUpsert,
			SubjectLabel: outcomegraph.LabelWorkCase,
			SubjectID:    "wc_1",
			Delta: outcomegraph.GraphDelta{Nodes: []outcomegraph.GraphNode{{
				Tenant: tenant, Label: outcomegraph.LabelWorkCase, BusinessID: "wc_1", Revision: uint64(i + 1),
			}}},
			SubjectRevision: uint64(i + 1),
		})
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
}
