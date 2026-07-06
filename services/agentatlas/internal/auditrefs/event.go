// Package auditrefs guarantees AgentAtlas actions reach the AgentNexus audit
// chain: a metrics-counting decorator over the nexus client plus the list of
// actions that must never skip auditing.
package auditrefs

import "github.com/astraclawteam/agentatlas/sdk/go/nexus"

// MustAuditActions enumerates actions that are invalid without an audit
// append; callers fail closed when appending fails.
var MustAuditActions = map[nexus.AuditAction]bool{
	nexus.AuditWorkflowDraftCreated:     true,
	nexus.AuditWorkflowVersionPublished: true,
	nexus.AuditDreamPolicyCreated:       true,
	nexus.AuditDreamJobRun:              true,
	nexus.AuditRetrievalPlanCreated:     true,
	nexus.AuditEvidenceLocated:          true,
	nexus.AuditEvidenceRead:             true,
	nexus.AuditAnswerTraceCreated:       true,
	nexus.AuditSensitiveArtifactParsed:  true,
	nexus.AuditVisibilityRuleChanged:    true,
}
