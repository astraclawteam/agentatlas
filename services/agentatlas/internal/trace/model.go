// Package trace persists Answer Traces and appends them to the AgentNexus
// audit chain. High-risk paths fail closed when the audit append fails.
package trace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Record is everything one answer must be able to prove.
type Record struct {
	EnterpriseID             string
	CaseTicketID             string
	ActorUserID              string
	Question                 string
	SanitizedQuestionSummary string
	WorkflowRunID            string
	SpaceIDs                 []string
	RetrievalPlanID          string
	EvidencePointerIDs       []string
	ReadGrantIDs             []string
	ModelRoute               string
	Answer                   string
	Steps                    []Step
	ModelLatencyMS           int64
}

type Step struct {
	Kind   string
	Detail map[string]any
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
