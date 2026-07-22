package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// defaultMetricsAddr matches the atlas-worker listener pattern
// (ATLAS_WORKER_METRICS_ADDR, :9091). This service had no listener of ANY kind:
// no /healthz, no /readyz, no metrics port, while every other long-lived service
// in both products has at least one.
const defaultMetricsAddr = ":9091"

// projectionHealth is what the projector knows about its own progress.
//
// The failure this exists for is silent by design: ProjectAll errors are logged
// at WARN and the watermark simply stops advancing, so the process keeps
// running, keeps ticking, and produces a graph that quietly goes stale. Its
// runtime image is alpine with one static binary and no psql, so nothing inside
// the container could be asked whether the projection was advancing either.
//
// Readiness is therefore the outcome of the last COMPLETED projection cycle,
// not the existence of the process. Lag is reported separately, as a number: a
// nonzero lag is normal between ticks and is not by itself unready, but a lag
// that stops falling is exactly what an operator needs to see, and until now
// there was no way to see it at all.
type projectionHealth struct {
	mu sync.Mutex
	// cycleErr is the error from the last completed ProjectAll, or nil. The
	// zero value is "no cycle has completed yet", which is NOT ready: a
	// projector that has never managed a pass has not proven anything.
	cycleErr    error
	cycleRan    bool
	lag         []outcomegraph.TenantLag
	lagErr      error
	lagObserved bool
}

func newProjectionHealth() *projectionHealth { return &projectionHealth{} }

// CycleCompleted records the outcome of one ProjectAll pass.
func (h *projectionHealth) CycleCompleted(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cycleRan = true
	h.cycleErr = err
}

// LagObserved records a lag reading, or the error that prevented one.
//
// A failed reading does NOT clear the last good one. A time series that
// disappears reads as "nothing to see" on most dashboards, and the moment the
// lag stops being readable is exactly the moment its last known value matters
// most. The staleness is reported through readiness instead.
func (h *projectionHealth) LagObserved(lag []outcomegraph.TenantLag, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lagErr = err
	if err != nil {
		return
	}
	h.lagObserved = true
	h.lag = lag
}

// Lag returns the last reading, for the metrics exporter.
func (h *projectionHealth) Lag() []outcomegraph.TenantLag {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]outcomegraph.TenantLag(nil), h.lag...)
}

// NotReadyReason states why the projection is not known to be advancing, or ""
// when the last pass succeeded. It names the condition and never the tenant: the
// listener is unauthenticated inside the deployment network and tenant ids are
// customer information.
func (h *projectionHealth) NotReadyReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !h.cycleRan:
		return "no projection pass has completed yet"
	case h.cycleErr != nil:
		// The graph store's own errors name a DSN. Report the condition.
		return "the last projection pass failed; the watermark is not advancing and the Outcome Graph is stale"
	case h.lagErr != nil:
		return "the projection lag could not be read; whether the graph is advancing is unknown"
	case !h.lagObserved:
		return "the projection lag has not been read yet"
	}
	return ""
}

// lagCollector exports the watermark and the lag behind it. It is a collector
// rather than a set of gauges written on each tick so a scrape can never read a
// half-updated tenant set.
type lagCollector struct {
	health    *projectionHealth
	watermark *prometheus.Desc
	head      *prometheus.Desc
	lag       *prometheus.Desc
}

func newLagCollector(health *projectionHealth) *lagCollector {
	return &lagCollector{
		health: health,
		watermark: prometheus.NewDesc("agentatlas_outcome_projection_watermark",
			"How far a tenant's projection outbox has been applied to the Outcome Graph.", []string{"tenant"}, nil),
		head: prometheus.NewDesc("agentatlas_outcome_projection_outbox_head",
			"The highest projection outbox sequence appended for a tenant.", []string{"tenant"}, nil),
		lag: prometheus.NewDesc("agentatlas_outcome_projection_lag_events",
			"Committed domain facts a tenant's Outcome Graph has not applied yet. A value that stops falling is a stalled projection.", []string{"tenant"}, nil),
	}
}

func (c *lagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.watermark
	ch <- c.head
	ch <- c.lag
}

func (c *lagCollector) Collect(ch chan<- prometheus.Metric) {
	for _, tenant := range c.health.Lag() {
		ch <- prometheus.MustNewConstMetric(c.watermark, prometheus.GaugeValue, float64(tenant.Watermark), tenant.Tenant)
		ch <- prometheus.MustNewConstMetric(c.head, prometheus.GaugeValue, float64(tenant.Head), tenant.Tenant)
		ch <- prometheus.MustNewConstMetric(c.lag, prometheus.GaugeValue, float64(tenant.Events), tenant.Tenant)
	}
}

// newProjectorHealthMux builds the listener this service never had.
//
// /healthz is liveness and always answers 200, so the container does not flap.
// /readyz answers 503 when the projection is not known to be advancing, which is
// what the compose healthcheck asks: a probe that only reported "the process
// exists" would be the same false green this whole change exists to remove.
// /metrics carries the lag, which is the number that says how bad it is.
func newProjectorHealthMux(health *projectionHealth) (*http.ServeMux, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(newLagCollector(health)); err != nil {
		return nil, fmt.Errorf("register projection lag metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeProjectorHealth(w, http.StatusOK, projectorStatus(true), "")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		reason := health.NotReadyReason()
		status := projectorStatus(reason == "")
		code := http.StatusOK
		// Branch on the value the body carries rather than re-deriving the
		// predicate, so the status an orchestrator sees and the body an
		// operator reads can never disagree.
		if !status.Ready {
			code = http.StatusServiceUnavailable
		}
		writeProjectorHealth(w, code, status, reason)
	})
	return mux, nil
}

func projectorStatus(ready bool) app.HealthStatus {
	return app.NewHealthStatus("atlas-outcome-projector", "postgres", "apache-age").MarkReady(ready)
}

func writeProjectorHealth(w http.ResponseWriter, code int, health app.HealthStatus, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The reason describes this service's own projection progress; it carries
	// no tenant identifier and no connection string.
	if reason == "" {
		fmt.Fprintf(w, `{"service":%q,"version":%q,"ready":%t,"dependencies":["apache-age","postgres"]}`,
			health.Service, health.Version, health.Ready)
		return
	}
	fmt.Fprintf(w, `{"service":%q,"version":%q,"ready":%t,"dependencies":["apache-age","postgres"],"reason":%q}`,
		health.Service, health.Version, health.Ready, reason)
}

// observeProjection runs one projection pass and records everything the health
// surface reports from it.
//
// The pass and the observation live together on purpose: the defect was a pass
// whose failure went nowhere, and separating "do the work" from "say what
// happened" is what let it go nowhere in the first place.
func observeProjection(ctx context.Context, projector *outcomegraph.Projector, health *projectionHealth) error {
	err := projector.ProjectAll(ctx)
	health.CycleCompleted(err)
	health.LagObserved(projector.Lag(ctx))
	return err
}
