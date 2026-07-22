package workcaseexec

// gap.go records, as code rather than prose, why two of the Orchestrator's four
// required seams have no production implementation. Each statement below is
// pinned by a test in gap_test.go against the SAME frozen AgentNexus contract
// snapshot the nexusclient parity tests use, so none of it can quietly rot: when
// AgentNexus closes one of these gaps the corresponding test goes red and this
// file has to change with it.
//
// The point of naming them is that "no ActionGateway exists" and "an
// ActionGateway cannot exist yet" are very different facts, and only the second
// one is true. Writing the adapter anyway would produce a gateway that can only
// ever be refused by a real AgentNexus -- another implemented, unit-tested,
// unreachable path, which is exactly what this work is clearing.

import "errors"

// ErrNoServiceCallableActionSurface reports that AgentNexus's frozen
// POST /v1/runtime/act accepts only PER-ACTOR credentials -- browserSession,
// browserAccessToken and caseTicket. AgentAtlas holds exactly one AgentNexus
// credential in a headless process: the trustedServiceSecret loaded by
// nexusclient.New. Every Access Ticket AgentAtlas ever holds arrives on an
// INBOUND request and is verified per-request (internal/app.verifyTicket); a
// background orchestrator has no inbound request and therefore no ticket, and
// AgentNexus publishes no operation that issues one to a first-party service.
//
// So the orchestrator's Dispatch has no credential it is allowed to present.
// Pinned by TestFrozenActSurfaceRefusesTheOnlyCredentialAHeadlessAtlasHolds.
var ErrNoServiceCallableActionSurface = errors.New(
	"workcaseexec: AgentNexus POST /v1/runtime/act accepts only per-actor credentials (browserSession/browserAccessToken/caseTicket); a headless AgentAtlas orchestrator holds only the trusted service secret and no surface issues it a Case Ticket")

// ErrNoRiskDecisionSigner reports that the frozen runtime.ActionRequest requires
// an embedded, SIGNED RiskDecision from the calling authority, bound to the same
// capability, parameter hash and business context. AgentAtlas holds no private
// signing key anywhere in this repository or its configuration -- every ed25519
// value it handles is a PUBLIC key used to VERIFY AgentNexus's signatures. It
// can therefore not author a valid ActionRequest at all.
//
// Pinned by TestAtlasCannotAuthorAValidFrozenActionRequest.
var ErrNoRiskDecisionSigner = errors.New(
	"workcaseexec: the frozen runtime.ActionRequest requires a signed RiskDecision bound to the exact operation, and AgentAtlas holds no signing key")

// ErrNoGovernanceFactsStore reports that the Governor has no persisted facts to
// expand a plan Step from. internal/governance models change drafts, review
// routes and decisions; it holds no capability registry, no capability ->
// side-effecting classification, and no per-step approval parties. The Governor
// interface's whole security value is that its sideEffecting flag comes from the
// business capability rather than the plan's LLM-influenceable Kind label, and
// there is nothing authoritative to derive it FROM yet.
var ErrNoGovernanceFactsStore = errors.New(
	"workcaseexec: no persisted governance facts exist to derive a trusted side-effecting classification or approval parties from (internal/governance stores change review, not capabilities)")

// seamReasons maps a required seam name to the contract-level reason it has no
// production source. A seam absent from this map is simply unwired by the
// caller, which is a different (and much more fixable) problem; keeping the two
// apart is why NotComposedError carries reasons at all.
func seamReasons() map[string]string {
	return map[string]string{
		"Gateway":  ErrNoServiceCallableActionSurface.Error() + "; and " + ErrNoRiskDecisionSigner.Error(),
		"Governor": ErrNoGovernanceFactsStore.Error(),
	}
}
