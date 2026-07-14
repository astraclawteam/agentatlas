package outcomelearning

import (
	"context"
	"encoding/json"

	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
)

// Distiller is the injected model PROPOSER port. It may PROPOSE a human-readable
// summary and a draft change payload from opaque evidence, but it can NEVER:
//
//   - publish, adopt or decide a candidate's lifecycle;
//   - read or write the Outcome Graph, the Outcome store or governance;
//   - influence the deterministic replay/shadow verdict or the source binding.
//
// Its entire output is UNTRUSTED and evidence-free: the service scrubs the
// summary/change (redacting any connector/credential/raw-query shape) and
// treats everything load-bearing as computed from immutable data. This is the
// model-boundary: the model proposes; the deterministic evidence-grounding +
// governance decides. GA tests use a deterministic/fake distiller; production
// wiring supplies an llmrouter-backed proposer, but nothing about that changes
// the guarantees here because the proposer output is never authoritative.
type Distiller interface {
	Propose(ctx context.Context, in ProposalInput) (Proposal, error)
}

// ProposalInput is the OPAQUE evidence the proposer sees: the kind under
// consideration, tenant/org scope, the subject reference and bounded permitted
// evidence summaries. It carries no connector endpoints, credentials or raw
// content, and the proposer cannot use it to reach any system — it is a
// read-only, opaque snapshot. Adversarial text inside EvidenceSummaries is
// expected and inert.
type ProposalInput struct {
	Kind      CandidateKind     `json:"kind"`
	Tenant    string            `json:"tenant"`
	Org       string            `json:"org,omitempty"`
	Subject   SourceRef         `json:"subject"`
	Watermark uint64            `json:"watermark"`
	Evidence  []EvidenceSummary `json:"evidence,omitempty"`
}

// EvidenceSummary is one opaque, bounded evidence line shown to the proposer.
type EvidenceSummary struct {
	Ref     SourceRef `json:"ref"`
	Summary string    `json:"summary,omitempty"`
}

// Proposal is the proposer's UNTRUSTED output. Only the (scrubbed) summary and
// change are ever used, and only as content for a governance draft — never as
// authority.
type Proposal struct {
	Summary string         `json:"summary,omitempty"`
	Change  ProposedChange `json:"change"`
}

// ProposedChange is the proposer's suggested governance change payload. The
// service overrides ResourceType/Action from the candidate kind's governance
// target (a proposer-supplied publish action is always discarded), and scrubs
// Content before handing it to review.
type ProposedChange struct {
	ResourceType govmodel.ResourceType `json:"resource_type,omitempty"`
	ResourceID   string                `json:"resource_id,omitempty"`
	Action       govmodel.Action       `json:"action,omitempty"`
	Content      json.RawMessage       `json:"content,omitempty"`
}

// StaticDistiller is a deterministic, model-free proposer. It is the safe
// default and the shape GA unit tests use: it echoes a fixed summary/content
// and never proposes a publish action. Because the pipeline ignores everything
// but the scrubbed summary/content, a deterministic proposer yields a fully
// deterministic candidate lifecycle.
type StaticDistiller struct {
	Summary string
	Content json.RawMessage
}

// Propose returns the static proposal.
func (d StaticDistiller) Propose(_ context.Context, _ ProposalInput) (Proposal, error) {
	content := d.Content
	if len(content) == 0 {
		content = json.RawMessage(`{"note":"distilled candidate for governed review"}`)
	}
	return Proposal{Summary: d.Summary, Change: ProposedChange{Content: content}}, nil
}

var _ Distiller = StaticDistiller{}
