package main

import (
	"errors"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcaseexec"
)

// workCaseOrchestrationEnv opts this worker into composing the WorkCase
// orchestrator. It is off by default because the orchestrator has two seams
// with no production source (see internal/workcaseexec/gap.go), and a worker
// that refused to start over an unbuildable optional capability would take the
// artifact, index and dream planes down with it.
//
// The switch is the point: with it OFF the worker states, once, that WorkCase
// orchestration is not composed and why. With it ON the worker MUST compose or
// REFUSE TO START -- an operator who asked for governed execution never gets a
// process that runs happily without it.
const workCaseOrchestrationEnv = "ATLAS_WORKCASE_ORCHESTRATION"

// composeWorkCaseOrchestrator is atlas-worker's WorkCase composition point, and
// the only one in any binary.
//
// It returns (nil, nil) when the capability is not requested, (orchestrator,
// nil) when it composed, and (nil, err) when it was requested and could not be
// composed -- which run() turns into a startup failure rather than a warning.
//
// The Orchestrator it returns is not DRIVEN here. Driving Advance from this
// worker is task C2; this function is the seam C2 calls, so that when C2 lands
// it adds a loop and not a second composition root.
func composeWorkCaseOrchestrator(cfg *config.Config, pool *pgxpool.Pool, logger *zap.Logger) (*workcase.Orchestrator, error) {
	requested := isTruthy(os.Getenv(workCaseOrchestrationEnv))

	orch, report, err := workcaseexec.Build(workcaseexec.BuildInput{
		Pool:           pool,
		TrustedKeyID:   cfg.AgentNexus.ActionSigningKeyID,
		TrustedKeyFile: cfg.AgentNexus.ActionSigningKeyFile,
		// CurrentOrgVersion, Gateway and Governor have no production source
		// today. They are passed as their zero values ON PURPOSE: the refusal
		// below is then derived from what was supplied, so the day a real
		// Gateway/Governor exists this call site is the only thing that changes.
	})
	if err != nil {
		if !requested {
			// Not requested: say it once, plainly, and carry on. This is the
			// line that turns "the feature silently does not exist" into "the
			// feature is declared, unavailable, and here is exactly why".
			logger.Warn("WorkCase orchestration is not composed",
				zap.String("enable_with", workCaseOrchestrationEnv+"=1"),
				zap.Error(err))
			logLimitations(logger, report)
			return nil, nil
		}
		return nil, err
	}
	logLimitations(logger, report)
	if !requested {
		// Composable but not asked for. Do not start it: an orchestrator that
		// drives governed side effects is not something a worker should switch
		// on because its dependencies happened to be present.
		logger.Info("WorkCase orchestration is composable but not enabled",
			zap.String("enable_with", workCaseOrchestrationEnv+"=1"))
		return nil, nil
	}
	logger.Info("WorkCase orchestration composed",
		zap.Bool("durable_run_ledger", report.DurableRunLedger))
	return orch, nil
}

// logLimitations publishes what the composed (or refused) orchestrator does not
// guarantee. A volatile run ledger contradicts the Orchestrator's own documented
// restart-safety, so it is stated every time rather than left in a doc comment.
func logLimitations(logger *zap.Logger, report workcaseexec.Report) {
	for _, limitation := range report.Limitations {
		logger.Warn("WorkCase orchestration limitation", zap.String("limitation", limitation))
	}
}

// workCaseStartupError renders a composition refusal for the startup path. It
// keeps the enumerated seam list from *workcaseexec.NotComposedError intact --
// "missing Gateway, Governor, TrustedKey" is the whole diagnostic, and a generic
// wrap would bury it.
func workCaseStartupError(err error) error {
	var notComposed *workcaseexec.NotComposedError
	if errors.As(err, &notComposed) {
		return errors.New(workCaseOrchestrationEnv + " is set but the orchestrator cannot be composed: " + notComposed.Error())
	}
	return err
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}
