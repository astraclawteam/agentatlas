package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// startMockNexusServer serves the frozen AgentNexus gateway-runtime contract
// over real HTTP for the DEPLOYED stack (containers reach the host via
// host.docker.internal). It is backed by the same nexusclient.Mock used by the
// in-process tests, so seeding and audit assertions stay identical.
func startMockNexusServer(t *testing.T, addr string, backing *nexusclient.Mock, events []nexus.OrgEvent) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("mock nexus listen %s: %v", addr, err)
	}
	server := &http.Server{Handler: newMockNexusHandler(backing, events), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	t.Logf("mock AgentNexus serving the frozen runtime contract at %s", addr)
}

// newMockNexusHandler builds the handler set without binding a port. The
// deployed-stack entry point above needs a fixed host address; the
// contract-conformance test needs the same handlers on loopback with no
// containers running. Both must be the SAME handlers or the conformance
// assertions would prove nothing about what the deployed stack talks to.
func newMockNexusHandler(backing *nexusclient.Mock, events []nexus.OrgEvent) http.Handler {
	mux := http.NewServeMux()
	approvals := newMockApprovalPlane()

	mux.HandleFunc("POST /v1/tickets/verify", func(w http.ResponseWriter, r *http.Request) {
		var req nexus.VerifyTicketRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, err := backing.VerifyTicket(r.Context(), req)
		if err != nil {
			resp = nexus.VerifyTicketResponse{Valid: false}
		}
		writeJSONMock(w, http.StatusOK, resp)
	})

	// The frozen evidence surface. Both handlers decode through the SDK's
	// canonical strict decoders rather than any local field check: those
	// decoders ARE what a real AgentNexus runs (services/agentnexus
	// evidence_handler.go calls the identically named functions), so the mock
	// cannot drift from production by construction.
	mux.HandleFunc("POST /v1/runtime/locate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := nexusruntime.DecodeEvidenceRequest(body)
		if err != nil {
			writeMockError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		handles := make([]nexusruntime.EvidenceHandle, 0, len(req.DataNeeds))
		for _, need := range req.DataNeeds {
			if !backing.EvidenceAllowed(need.NeedID) {
				continue
			}
			handles = append(handles, nexusruntime.EvidenceHandle{
				EvidenceRef: "evd_" + padRef(need.NeedID), DataClass: need.DataClass,
			})
		}
		writeJSONMock(w, http.StatusOK, map[string]any{
			"business_context_ref": "wc_" + padRef("mocknexus"),
			"evidence":             handles,
		})
	})

	mux.HandleFunc("POST /v1/runtime/read", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := nexusruntime.DecodeEvidenceReadRequest(body)
		if err != nil {
			writeMockError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		detail, ok := backing.EvidenceDetail(req.EvidenceRef)
		if !ok {
			writeMockError(w, http.StatusForbidden, "evidence_denied") // fail closed
			return
		}
		// The read envelope is the real handler's envelope, member for member:
		// decision, then — on an allow only — the data and the freshness trio,
		// so a cached read can never masquerade as real-time.
		//
		// There is deliberately NO grant_ref. The frozen EvidenceReadResponse
		// schema lists the member as optional, and the real AgentNexus read
		// handler never populates it: reads are served from evidence staged and
		// authorized at locate time, and a Step Grant is a separate audited
		// object minted by POST /v1/step-grants. Emitting one here (under a
		// grn_ prefix that is not even in the frozen handle grammar, whose
		// grant pattern is ^grant_[A-Za-z0-9_-]{16,128}$) hid a real
		// divergence: AgentAtlas's two dream-evidence handlers fail closed on
		// an empty GrantRef, so against a real AgentNexus they always 403.
		writeJSONMock(w, http.StatusOK, map[string]any{
			"decision":       "allow",
			"data":           map[string]any{"detail": detail},
			"source_version": 1, "as_of": "2026-07-21T00:00:00Z",
			"served_from_cache": false,
		})
	})

	mux.HandleFunc("POST /v1/audit/evidence", func(w http.ResponseWriter, r *http.Request) {
		var req nexus.AppendAuditEvidenceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, err := backing.AppendAuditEvidence(r.Context(), req)
		if err != nil {
			writeMockError(w, http.StatusInternalServerError, "audit_unavailable")
			return
		}
		writeJSONMock(w, http.StatusCreated, resp)
	})

	// The frozen approval TRANSMISSION surface. AgentAtlas's governed-change
	// route resolver calls both of these operations, so leaving them unserved
	// meant the path had zero end-to-end coverage. Transmission is currently
	// DORMANT by design — governance.WorkCaseContextFor returns false until
	// WorkCase-backed changes exist under task C1 — but the surface has to be
	// exercisable now, or the day C1 lands is the day it is first tested.
	//
	// Only the two operations AgentAtlas actually calls are served. The
	// revocation and evidence-recording operations exist on the frozen contract
	// but have no AgentAtlas consumer, and a mock endpoint nobody calls is
	// unexercised code that drifts exactly like the rest of this file did.
	mux.HandleFunc("POST /v1/approvals/transmissions", func(w http.ResponseWriter, r *http.Request) {
		if !mockCredentialPresent(r) {
			writeMockError(w, http.StatusUnauthorized, "request_failed")
			return
		}
		body, _ := io.ReadAll(r.Body)
		req, err := nexusruntime.DecodeApprovalRequest(body)
		if err != nil {
			writeMockError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		// Two rejections the real service makes after Validate() passes: an
		// already-expired plan and a non-canonical authority.
		if !req.ExpiresAt.After(time.Now().UTC()) || strings.TrimSpace(req.Plan.Authority) != req.Plan.Authority {
			writeMockError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		status, conflict := approvals.transmit(req)
		if conflict {
			writeMockError(w, http.StatusConflict, "approval_conflict")
			return
		}
		writeJSONMock(w, http.StatusOK, status)
	})

	mux.HandleFunc("GET /v1/approvals/transmissions/{plan_ref}", func(w http.ResponseWriter, r *http.Request) {
		if !mockCredentialPresent(r) {
			writeMockError(w, http.StatusUnauthorized, "request_failed")
			return
		}
		status, ok := approvals.status(r.PathValue("plan_ref"))
		if !ok {
			writeMockError(w, http.StatusNotFound, "approval_not_found")
			return
		}
		writeJSONMock(w, http.StatusOK, status)
	})

	mux.HandleFunc("GET /v1/org-events", func(w http.ResponseWriter, r *http.Request) {
		// The frozen operation is security:[{trustedServiceSecret}]; the real
		// handler 401s anything that is not a verified service credential. A
		// mock that skips this is more permissive than production.
		if _, _, ok := r.BasicAuth(); !ok {
			writeMockError(w, http.StatusUnauthorized, "invalid_service")
			return
		}
		// since_version is the ONLY declared parameter. Tenant scope comes from
		// the credential, so a caller-supplied enterprise_id is a forbidden org
		// fact and the mock must refuse it rather than quietly filter on it.
		if r.URL.Query().Has("enterprise_id") {
			writeMockError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_version"), 10, 64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			if ev.OrgVersion <= since {
				continue
			}
			raw, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\nevent: org\ndata: %s\n\n", ev.OrgVersion, raw)
		}
		if flusher != nil {
			flusher.Flush()
		}
		// Hold the stream open so the worker's resume loop idles instead of
		// hammering reconnects; heartbeats keep intermediaries from timing out.
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, ": keepalive\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})

	return mux
}

// mockApprovalPlane is the in-memory transmission store behind the approval
// endpoints. The real service keys a transmission by (tenant, plan_ref); the
// mock serves exactly one tenant, so plan_ref alone is the key.
//
// The stored value is nexusclient.ApprovalTransmissionStatus — the type
// AgentAtlas decodes into, which mirrors the frozen schema member for member.
// Serving it means no approver identity, queue or risk field is representable
// in a mock response, which is the boundary the frozen schema states in words.
type mockApprovalPlane struct {
	mu            sync.Mutex
	transmissions map[string]nexusclient.ApprovalTransmissionStatus
}

func newMockApprovalPlane() *mockApprovalPlane {
	return &mockApprovalPlane{transmissions: map[string]nexusclient.ApprovalTransmissionStatus{}}
}

// transmit applies the frozen idempotency rule: an identical re-transmit is
// idempotent, a still-pending plan is re-delivered, every later state is
// returned untouched, and a plan_ref rebound to a different operation or plan
// digest is a conflict. It reports conflict rather than returning an error so
// the caller can map it to the declared 409.
func (p *mockApprovalPlane) transmit(req nexusruntime.ApprovalRequest) (nexusclient.ApprovalTransmissionStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	candidate := nexusclient.ApprovalTransmissionStatus{
		PlanRef:            req.Plan.PlanRef,
		PlanHash:           req.Plan.PlanHash,
		Authority:          req.Plan.Authority,
		BusinessContextRef: req.BusinessContextRef,
		Capability:         req.Capability,
		ParameterHash:      req.ParameterHash,
		ExpiresAt:          req.ExpiresAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:          now,
	}
	if existing, ok := p.transmissions[req.Plan.PlanRef]; ok {
		if existing.PlanHash != candidate.PlanHash || existing.Authority != candidate.Authority ||
			existing.BusinessContextRef != candidate.BusinessContextRef ||
			existing.Capability != candidate.Capability || existing.ParameterHash != candidate.ParameterHash {
			return nexusclient.ApprovalTransmissionStatus{}, true
		}
		if existing.Status != nexusclient.TransmissionPending {
			return existing, false
		}
		existing.DeliveryAttempts++
		existing.Status = nexusclient.TransmissionDelivered
		existing.LastDeliveryState = "delivered"
		existing.UpdatedAt = now
		p.transmissions[req.Plan.PlanRef] = existing
		return existing, false
	}
	// A first transmit records the correlation and attempts one delivery. The
	// mock's channel always succeeds, so the plan lands on "delivered" —
	// delivery, NOT a decision: only evidence_recorded carries one, and nothing
	// in this mock can produce it, because only the external approval authority
	// can.
	candidate.Status = nexusclient.TransmissionDelivered
	candidate.DeliveryAttempts = 1
	candidate.LastDeliveryState = "delivered"
	p.transmissions[req.Plan.PlanRef] = candidate
	return candidate, false
}

func (p *mockApprovalPlane) status(planRef string) (nexusclient.ApprovalTransmissionStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status, ok := p.transmissions[planRef]
	return status, ok
}

// mockCredentialPresent reports whether the request carries any credential the
// frozen operations declare (a trusted service secret over basic auth, a
// browser BFF bearer token, or a Case Ticket). The real ingress resolves a
// trusted context from a VERIFIED credential and 401s a request carrying none;
// a mock that served anonymous callers would be more permissive than
// production. The mock cannot verify secrets it was never given, so it checks
// presence and shape only — which is exactly the divergence class that matters
// here, since AgentAtlas's failure mode was sending no credential at all.
func mockCredentialPresent(r *http.Request) bool {
	if _, _, ok := r.BasicAuth(); ok {
		return true
	}
	authorization := r.Header.Get("Authorization")
	return strings.HasPrefix(authorization, "Bearer ") || strings.HasPrefix(authorization, "CaseTicket ")
}

func writeJSONMock(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeMockError mirrors the real gateway's failure envelope: a fixed, opaque
// coded reason that never echoes caller content back.
func writeMockError(w http.ResponseWriter, status int, reason string) {
	writeJSONMock(w, status, map[string]string{"error": reason})
}

// padRef pads a seed into the >=16-character opaque handle grammar the frozen
// contract requires, so the mock's handles pass the same validation a real one
// would.
func padRef(seed string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, seed)
	for len(cleaned) < 16 {
		cleaned += "0"
	}
	if len(cleaned) > 128 {
		cleaned = cleaned[:128]
	}
	return cleaned
}
