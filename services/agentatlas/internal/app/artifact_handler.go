package app

import (
	"io"
	"net/http"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
)

// maxArtifactUploadBytes bounds one artifact upload through the runtime API.
const maxArtifactUploadBytes = 512 << 20

// artifactHandler serves artifact ingestion + processing-job creation
// (POST /v1/artifacts/jobs). Processing itself runs on atlas-worker.
type artifactHandler struct {
	nexus     nexus.Client
	artifacts *artifacts.Service
}

func (h *artifactHandler) createJob(w http.ResponseWriter, r *http.Request) {
	ticketID, ticket, err := verifyTicket(r.Context(), h.nexus, r)
	_ = ticketID
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	if h.artifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "artifacts_unavailable", "artifact service not configured")
		return
	}
	if err := r.ParseMultipartForm(maxArtifactUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "bad_multipart", err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_file", "multipart field 'file' is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxArtifactUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed", err.Error())
		return
	}
	if len(data) > maxArtifactUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds limit")
		return
	}
	contentType := r.FormValue("content_type")
	if contentType == "" {
		contentType = header.Header.Get("Content-Type")
	}

	artifact, err := h.artifacts.IngestUpload(r.Context(), ticket.EnterpriseID, header.Filename, contentType, data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ingest_failed", err.Error())
		return
	}
	job, err := h.artifacts.CreateJob(r.Context(), ticket.EnterpriseID, artifact.ID, r.FormValue("parser_hint"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"artifact_id": artifact.ID, "job_id": job.ID, "status": job.Status,
	})
}
