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

// ReadEvidenceResult is the decoded EvidenceReadResponse. The freshness trio
// (SourceVersion / AsOf / ServedFromCache) is always present on an allowed
// read: a cached answer must never masquerade as a live one, so AgentAtlas
// carries the disclosure through to its Answer Trace rather than dropping it.
type ReadEvidenceResult struct {
	Decision        string         `json:"decision"`
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

// Locate resolves declared business-semantic data needs to opaque evidence
// handles. It is the frozen-contract replacement for LocateEvidence.
func (c *HTTPClient) Locate(ctx context.Context, req nexusruntime.EvidenceRequest) (LocateEvidenceResult, error) {
	var out LocateEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence request: %w", err)
	}
	err := c.post(ctx, "/v1/runtime/locate", req, &out)
	return out, err
}

// Read reads one located evidence handle under policy. It is the
// frozen-contract replacement for ReadEvidence.
func (c *HTTPClient) Read(ctx context.Context, req nexusruntime.EvidenceReadRequest) (ReadEvidenceResult, error) {
	var out ReadEvidenceResult
	if err := req.Validate(); err != nil {
		return out, fmt.Errorf("nexus evidence read request: %w", err)
	}
	err := c.post(ctx, "/v1/runtime/read", req, &out)
	return out, err
}
