// Package observability provides structured logging (zap) and, from Goal 13,
// OpenTelemetry tracing and Prometheus metrics.
package observability

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewLogger builds the production JSON logger with the service name attached
// to every entry. Log fields follow the Goal 13 conventions (enterprise_id,
// request_id, trace_id, ticket_id, workflow_run_id, space_id) as they appear.
func NewLogger(service string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build(zap.Fields(zap.String("service", service)))
}
