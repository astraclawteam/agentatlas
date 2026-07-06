package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
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

	mux.HandleFunc("POST /v1/evidence/locate", func(w http.ResponseWriter, r *http.Request) {
		var req nexus.LocateEvidenceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, err := backing.LocateEvidence(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden) // fail closed
			return
		}
		writeJSONMock(w, http.StatusOK, resp)
	})

	mux.HandleFunc("POST /v1/evidence/read", func(w http.ResponseWriter, r *http.Request) {
		var req nexus.ReadEvidenceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, err := backing.ReadEvidence(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden) // fail closed
			return
		}
		writeJSONMock(w, http.StatusOK, resp)
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
