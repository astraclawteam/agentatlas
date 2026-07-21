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
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// startMockNexusServer serves the agentnexus-client.yaml proposal contract
// over real HTTP for the DEPLOYED stack (containers reach the host via
// host.docker.internal). It is backed by the same nexusclient.Mock used by
// the in-process tests, so seeding and audit assertions stay identical.
func startMockNexusServer(t *testing.T, addr string, backing *nexusclient.Mock, events []nexus.OrgEvent) {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/tickets/verify", func(w http.ResponseWriter, r *http.Request) {
		var req nexus.VerifyTicketRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, err := backing.VerifyTicket(r.Context(), req)
		if err != nil {
			resp = nexus.VerifyTicketResponse{Valid: false}
		}
		writeJSONMock(w, http.StatusOK, resp)
	})

	// The frozen evidence surface. This mock is deliberately strict about the
	// REQUEST it accepts: the whole point of the migration is that identity and
	// connector topology no longer travel in the body, so a mock that tolerated
	// them would let a regression pass here and fail against a real AgentNexus.
	mux.HandleFunc("POST /v1/runtime/locate", func(w http.ResponseWriter, r *http.Request) {
		var req nexusruntime.EvidenceRequest
		body, _ := io.ReadAll(r.Body)
		if rejectRetiredEvidenceFields(w, body) {
			return
		}
		_ = json.Unmarshal(body, &req)
		if err := req.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		var req nexusruntime.EvidenceReadRequest
		body, _ := io.ReadAll(r.Body)
		if rejectRetiredEvidenceFields(w, body) {
			return
		}
		_ = json.Unmarshal(body, &req)
		if err := req.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		detail, ok := backing.EvidenceDetail(req.EvidenceRef)
		if !ok {
			http.Error(w, "no evidence under handle", http.StatusForbidden) // fail closed
			return
		}
		// The freshness trio is always present on an allowed read.
		writeJSONMock(w, http.StatusOK, map[string]any{
			"decision": "allow", "grant_ref": "grn_" + padRef(req.EvidenceRef),
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONMock(w, http.StatusCreated, resp)
	})

	mux.HandleFunc("GET /v1/org-events", func(w http.ResponseWriter, r *http.Request) {
		enterprise := r.URL.Query().Get("enterprise_id")
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_version"), 10, 64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			if ev.EnterpriseID != enterprise || ev.OrgVersion <= since {
				continue
			}
			raw, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", raw)
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

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("mock nexus listen %s: %v", addr, err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	t.Logf("mock AgentNexus serving the proposal contract at %s", addr)
}

func writeJSONMock(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rejectRetiredEvidenceFields fails any request that still carries identity or
// connector topology in its body. A mock that quietly tolerated them would let
// a regression pass end-to-end here and only fail against a real AgentNexus,
// which is precisely how the drift this migration fixed went unnoticed.
func rejectRetiredEvidenceFields(w http.ResponseWriter, body []byte) bool {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "malformed evidence request", http.StatusBadRequest)
		return true
	}
	for _, retired := range []string{"ticket_id", "enterprise_id", "resource_uri", "evidence_pointer_id", "query_intent", "max_bytes"} {
		if _, present := raw[retired]; present {
			http.Error(w, "retired field on the frozen evidence contract: "+retired, http.StatusBadRequest)
			return true
		}
	}
	return false
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
