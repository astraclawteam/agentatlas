package app

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// maxParseUploadBytes bounds one parse upload (matches the largest provider
// MaxFileSizeMB ceiling; the gateway re-checks per provider).
const maxParseUploadBytes = 512 << 20

// ParserRouterDeps is the composition surface for the parser-gateway service.
type ParserRouterDeps struct {
	Gateway *parsergateway.Gateway
	// Sidecars maps sidecar name -> base URL for /healthz reachability.
	Sidecars map[string]string
	Metrics  *observability.Metrics // optional
}

// NewParserRouter builds the parser-gateway HTTP surface: POST /v1/parse fans
// out to the registered providers; /healthz reports per-sidecar reachability.
func NewParserRouter(deps ParserRouterDeps) *chi.Mux {
	r := chi.NewRouter()
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
		r.Method(http.MethodGet, "/metrics", deps.Metrics.Handler())
	}

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		status := NewHealthStatus("parser-gateway", "docling", "mineru", "asr", "video").MarkReady(true)
		reachability := map[string]bool{}
		for name, base := range deps.Sidecars {
			reachability[name] = sidecarReachable(req.Context(), base)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"health":   status,
			"sidecars": reachability,
		})
	})

	r.Post("/v1/parse", func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseMultipartForm(maxParseUploadBytes); err != nil {
			writeError(w, http.StatusBadRequest, "bad_multipart", err.Error())
			return
		}
		file, header, err := req.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing_file", "multipart field 'file' is required")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxParseUploadBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read_failed", err.Error())
			return
		}
		if len(data) > maxParseUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds gateway limit")
			return
		}
		contentType := req.FormValue("content_type")
		if contentType == "" {
			contentType = header.Header.Get("Content-Type")
		}
		in := parsergateway.ParseInput{
			EnterpriseID: req.FormValue("enterprise_id"),
			ArtifactID:   req.FormValue("artifact_id"),
			Filename:     header.Filename,
			ContentType:  contentType,
			Data:         data,
		}
		result, err := deps.Gateway.Parse(req.Context(), req.FormValue("parser_hint"), in)
		if err != nil {
			// Unknown content types / provider failures fail loud.
			writeError(w, http.StatusUnprocessableEntity, "parse_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, parsergateway.ParseResponse{
			ProviderID: result.Output.ProviderID,
			LatencyMS:  result.LatencyMS,
			Output:     result.Output,
		})
	})

	return r
}

// sidecarReachable treats ANY HTTP response (404 included) as reachable —
// sidecars expose different health paths; transport failure means down.
func sidecarReachable(ctx context.Context, baseURL string) bool {
	if baseURL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
