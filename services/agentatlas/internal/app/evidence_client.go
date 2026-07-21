package app

import (
	"context"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// FrozenEvidenceClient is the frozen-contract evidence surface consumed by the
// handlers.
//
// It is declared here, on the consumer side, rather than in sdk/go/nexus:
// the response envelopes live in internal/nexusclient because AgentNexus's
// public SDK publishes the request vocabulary and EvidenceHandle but not the
// two response shapes, and the SDK package cannot import an internal one.
//
// It is a separate interface from nexus.Client for the same reason
// ApprovalClient and OrgAuthorizationClient are: a test double that only knows
// the retired evidence surface keeps compiling while the migration proceeds
// call site by call site.
type FrozenEvidenceClient interface {
	Locate(ctx context.Context, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error)
	Read(ctx context.Context, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error)
}

// evidenceRequestTTL bounds how long a located evidence request stays valid.
// The frozen contract requires an explicit expiry: an evidence request with no
// deadline is a standing authorization, which is precisely what the Access
// Ticket model exists to avoid.
const evidenceRequestTTL = 5 * time.Minute

// dreamEvidenceDataClass and dreamEvidencePurpose name the business-semantic
// data class and purpose of a Dream evidence drill-down. They replace the
// retired surface's resource URI and free-text query intent: the frozen
// contract addresses data by class and purpose, never by connector location.
const (
	dreamEvidenceDataClass = "dream.evidence"
	dreamEvidencePurpose   = "dream_evidence_drilldown"
)

// answerEvidenceDataClass and answerEvidencePurpose name the data class and
// purpose of an employee-answer evidence read.
const (
	answerEvidenceDataClass = "knowledge.evidence"
	answerEvidencePurpose   = "answer_employee_question"
)
