package nexusclient

import (
	"context"
	"fmt"

	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// The frozen-contract evidence surface.
//
// AgentNexus publishes its request vocabulary and the opaque EvidenceHandle in
// sdk/go/runtime, but not the two response envelopes, so they are decoded here
// against the published EvidenceLocateResponse / EvidenceReadResponse schemas.
// That gap is worth closing on the provider side later; defining the envelopes
// here does NOT re-create a parallel contract, because every type they are
// built from comes from the published SDK.
//
// The shape difference from the retired surface is the whole point of the
// migration: AgentAtlas no longer says "here is a ticket, an enterprise and a
// resource URI, read it". It declares WHAT it needs (a data class and a
// purpose) and receives opaque handles; identity comes from the verified
// service credential at ingress, and connector topology never appears.

// LocateEvidenceResult is the decoded EvidenceLocateResponse.
type LocateEvidenceResult struct {
	BusinessContextRef string                        `json:"business_context_ref"`
	Evidence           []nexusruntime.EvidenceHandle `json:"evidence"`
}

// Frozen PolicyDecision members of a read. AgentNexus emits allow and deny
// today; the other two are declared by the frozen enum and are matched here so
// a server that starts emitting them is understood rather than misread.
const (
	ReadAllow               = "allow"
	ReadDeny                = "deny"
	ReadNeedExternalReceipt = "need_external_receipt"
	ReadAllowWithMasking    = "allow_with_masking"
)

// ReadEvidenceResult is the decoded EvidenceReadResponse. The freshness trio
// (SourceVersion / AsOf / ServedFromCache) is always present on an allowed
// read: a cached answer must never masquerade as a live one, so AgentAtlas
// carries the disclosure through to its Answer Trace rather than dropping it.
type ReadEvidenceResult struct {
	Decision string `json:"decision"`
	// GrantRef is declared optional by the frozen EvidenceReadResponse schema
	// and is NEVER populated by AgentNexus: the read handler answers with the
	// decision, the data and the freshness trio, because a read is served from
	// evidence authorized at locate time. A Step Grant is a separate audited
	// object minted by POST /v1/step-grants.
	//
	// Do not gate anything on it. Two handlers did, and because a mock filled
	// it in they were dead against a real AgentNexus without anyone noticing.
	// Decision is the member that carries the authorization verdict, and the
	// schema marks it required; use Allowed.
	GrantRef        string         `json:"grant_ref,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
	ReceiptRef      string         `json:"receipt_ref,omitempty"`
	SourceVersion   int64          `json:"source_version,omitempty"`
	AsOf            string         `json:"as_of,omitempty"`
	ServedFromCache bool           `json:"served_from_cache,omitempty"`
	// ContinuationRef is present exactly when constraints bounded the read.
	// Results are never silently truncated, so an ignored continuation is a
	// caller bug, not a complete result.
	ContinuationRef string `json:"continuation_ref,omitempty"`
}

// Allowed reports whether the decision permits the caller to use the read.
//
// A denied read is NOT a transport error: AgentNexus answers 200 with
// {"decision":"deny"} and no data, so a caller that only checks err treats a
// refusal as an empty success and can go on to render, audit or feed a model
// with nothing. Every consumer of a read must gate on this.
func (r ReadEvidenceResult) Allowed() bool {
	return r.Decision == ReadAllow || r.Decision == ReadAllowWithMasking
}

// Locate resolves declared business-semantic data needs to opaque evidence
// handles. It is the frozen-contract replacement for LocateEvidence.
//
// ticketID is required and is NOT optional politeness. locateRuntimeEvidence
// declares security [{browserSession}, {browserAccessToken}, {caseTicket}] and
// deliberately omits trustedServiceSecret: this is a per-actor authorization
// surface, so WHO is asking must come from a verified actor credential. The
// ticket is a parameter rather than a body member because the body declares
// only WHAT is needed; and it is a required parameter rather than an optional
// one so that "send nothing at all" — the state this call was actually in —
// cannot be expressed. Callers holding a browser token use LocateWithBearer.
func (c *HTTPClient) Locate(ctx context.Context, ticketID string, req nexusruntime.EvidenceRequest) (LocateEvidenceResult, error) {
	var out LocateEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence request: %w", err)
	}
	err := c.ticketPost(ctx, "/v1/runtime/locate", ticketID, "", req, &out, nil)
	return out, err
}

// Read reads one located evidence handle under policy. It is the
// frozen-contract replacement for ReadEvidence. ticketID is required for the
// same reason it is on Locate.
func (c *HTTPClient) Read(ctx context.Context, ticketID string, req nexusruntime.EvidenceReadRequest) (ReadEvidenceResult, error) {
	var out ReadEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence read request: %w", err)
	}
	err := c.ticketPost(ctx, "/v1/runtime/read", ticketID, "", req, &out, nil)
	return out, err
}

// LocateWithBearer and ReadWithBearer are the browser-session variants. The
// console BFF holds an opaque browser access token rather than the service
// credential, but the request shape is identical: the token establishes WHO is
// asking, and the body still declares only WHAT is needed.
func (c *HTTPClient) LocateWithBearer(ctx context.Context, accessToken string, req nexusruntime.EvidenceRequest) (LocateEvidenceResult, error) {
	var out LocateEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence request: %w", err)
	}
	err := c.bearerPost(ctx, "/v1/runtime/locate", accessToken, "", req, &out, nil)
	return out, err
}

func (c *HTTPClient) ReadWithBearer(ctx context.Context, accessToken string, req nexusruntime.EvidenceReadRequest) (ReadEvidenceResult, error) {
	var out ReadEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence read request: %w", err)
	}
	err := c.bearerPost(ctx, "/v1/runtime/read", accessToken, "", req, &out, nil)
	return out, err
}
