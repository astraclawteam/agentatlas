package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcaseexec"
)

// These assertions drive composeWorkCaseOrchestrator itself. They deliberately
// do NOT read main.go's source text: a test that greps a composition root for a
// call proves a string is present and nothing about what the process does. The
// call site in run() is a single line whose behaviour is entirely this function.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://atlas:atlas@127.0.0.1:1/agentatlas?sslmode=disable")
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func recordingLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// Off by default, the worker must still SAY the capability is absent and why.
// Before this package existed, the WorkCase orchestrator was invisible: no
// binary mentioned it, so "the feature does not exist" and "the feature is
// working" produced identical output.
func TestWorkerStatesWhyWorkCaseOrchestrationIsAbsent(t *testing.T) {
	t.Setenv(workCaseOrchestrationEnv, "")
	logger, logs := recordingLogger(t)

	orch, err := composeWorkCaseOrchestrator(&config.Config{}, testPool(t), logger)
	if err != nil {
		t.Fatalf("an unrequested capability must not fail startup: %v", err)
	}
	if orch != nil {
		t.Fatal("an unrequested orchestrator must not be composed")
	}
	entries := logs.FilterMessage("WorkCase orchestration is not composed").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one statement that the capability is absent, got %d", len(entries))
	}
	// The statement is only useful if it names the seams and the switch.
	rendered := entries[0].Message
	for _, field := range entries[0].Context {
		rendered += " " + field.String
		if field.Interface != nil {
			if err, ok := field.Interface.(error); ok {
				rendered += " " + err.Error()
			}
		}
	}
	for _, want := range []string{"Gateway", "Governor", workCaseOrchestrationEnv} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the absence statement does not mention %s: %s", want, rendered)
		}
	}
}

// The volatile run ledger contradicts the Orchestrator's documented restart
// safety, so it must reach an operator whether or not the capability composed.
func TestWorkerPublishesTheVolatileRunLedgerLimitation(t *testing.T) {
	t.Setenv(workCaseOrchestrationEnv, "")
	logger, logs := recordingLogger(t)

	if _, err := composeWorkCaseOrchestrator(&config.Config{}, testPool(t), logger); err != nil {
		t.Fatalf("compose: %v", err)
	}
	entries := logs.FilterMessage("WorkCase orchestration limitation").All()
	if len(entries) == 0 {
		t.Fatal("the in-memory run ledger must be published as a limitation")
	}
	found := false
	for _, entry := range entries {
		for _, field := range entry.Context {
			if strings.Contains(field.String, "IN-MEMORY") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no limitation names the volatile run ledger: %v", entries)
	}
}

// This is the guard that matters: an operator who ASKED for governed execution
// must not get a worker that runs happily without it. Requested-but-unbuildable
// is a startup failure, and the failure names every absent seam.
func TestWorkerRefusesToStartWhenOrchestrationIsRequestedButUnbuildable(t *testing.T) {
	t.Setenv(workCaseOrchestrationEnv, "1")
	logger, _ := recordingLogger(t)

	orch, err := composeWorkCaseOrchestrator(&config.Config{}, testPool(t), logger)
	if err == nil {
		t.Fatal("requesting an unbuildable capability must fail, not warn")
	}
	if orch != nil {
		t.Fatal("a failed composition must not return an orchestrator")
	}
	var notComposed *workcaseexec.NotComposedError
	if !errors.As(err, &notComposed) {
		t.Fatalf("want *workcaseexec.NotComposedError, got %T", err)
	}
	startup := workCaseStartupError(err)
	for _, want := range []string{workCaseOrchestrationEnv, "Gateway", "Governor"} {
		if !strings.Contains(startup.Error(), want) {
			t.Errorf("the startup error does not name %s: %s", want, startup)
		}
	}
}

func TestIsTruthy(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", " on ", "yes"} {
		if !isTruthy(on) {
			t.Errorf("%q must enable the capability", on)
		}
	}
	for _, off := range []string{"", "0", "false", "off", "no", "maybe"} {
		if isTruthy(off) {
			t.Errorf("%q must not enable the capability", off)
		}
	}
}
