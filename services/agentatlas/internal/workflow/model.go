// Package workflow implements the workflow runtime: schema-validated drafts,
// immutable published versions, and auditable runs with per-node events.
package workflow

import (
	"crypto/rand"
	"encoding/hex"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

// Run statuses.
const (
	RunPending             = "pending"
	RunRunning             = "running"
	RunWaitingConfirmation = "waiting_confirmation"
	RunSucceeded           = "succeeded"
	RunFailed              = "failed"
	RunCancelled           = "cancelled"
)

// Node statuses recorded in workflow_run_events.
const (
	NodePending             = "pending"
	NodeRunning             = "running"
	NodeWaitingConfirmation = "waiting_confirmation"
	NodeSucceeded           = "succeeded"
	NodeFailed              = "failed"
	NodeSkipped             = "skipped"
)

// Definition is the sdk workflow document persisted as JSONB.
type Definition = sdkworkflow.Workflow

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}
