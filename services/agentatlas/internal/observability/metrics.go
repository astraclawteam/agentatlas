package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Metrics is the shared Prometheus surface. SaaS and private deployments
// scrape the same names.
type Metrics struct {
	registry *prometheus.Registry

	RequestLatency  *prometheus.HistogramVec
	RetrievalLatency prometheus.Histogram
	ParserLatency    prometheus.Histogram
	DreamRuns        *prometheus.CounterVec
	WorkflowRuns     *prometheus.CounterVec
	TraceAppendFail  prometheus.Counter
	AuditAppendFail  prometheus.Counter
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)
	return &Metrics{
		registry: reg,
		RequestLatency: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name: "agentatlas_request_duration_seconds", Help: "HTTP request latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "status"}),
		RetrievalLatency: factory.NewHistogram(prometheus.HistogramOpts{
			Name: "agentatlas_retrieval_duration_seconds", Help: "Retrieval plan execution latency",
			Buckets: prometheus.DefBuckets,
		}),
		ParserLatency: factory.NewHistogram(prometheus.HistogramOpts{
			Name: "agentatlas_parser_duration_seconds", Help: "Parser provider latency",
			Buckets: []float64{0.5, 1, 5, 15, 60, 300, 900},
		}),
		DreamRuns: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "agentatlas_dream_runs_total", Help: "Dream job runs by status",
		}, []string{"status"}),
		WorkflowRuns: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "agentatlas_workflow_runs_total", Help: "Workflow runs by status",
		}, []string{"status"}),
		TraceAppendFail: factory.NewCounter(prometheus.CounterOpts{
			Name: "agentatlas_trace_append_failures_total", Help: "Answer trace persistence failures",
		}),
		AuditAppendFail: factory.NewCounter(prometheus.CounterOpts{
			Name: "agentatlas_audit_append_failures_total", Help: "AgentNexus audit append failures (fail-closed events)",
		}),
	}
}

// Handler exposes /metrics for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Middleware records per-route request latency.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.RequestLatency.WithLabelValues(r.URL.Path, strconv.Itoa(rec.status)).
			Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// RequestLogger attaches the canonical correlation fields. Field names are a
// stable contract: enterprise_id, request_id, trace_id, ticket_id,
// workflow_run_id, space_id.
func RequestLogger(base *zap.Logger, enterpriseID, requestID, traceID, ticketID, workflowRunID, spaceID string) *zap.Logger {
	fields := make([]zap.Field, 0, 6)
	add := func(key, v string) {
		if v != "" {
			fields = append(fields, zap.String(key, v))
		}
	}
	add("enterprise_id", enterpriseID)
	add("request_id", requestID)
	add("trace_id", traceID)
	add("ticket_id", ticketID)
	add("workflow_run_id", workflowRunID)
	add("space_id", spaceID)
	return base.With(fields...)
}
