package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder produces vectors for indexing and querying. Production routes
// through llmrouter's OpenAI-compatible /embeddings (bge-m3, 1024 dims —
// pinned in mapping.json).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
}

const EmbeddingDimension = 1024

type LLMRouterEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

var _ Embedder = (*LLMRouterEmbedder)(nil)

func NewLLMRouterEmbedder(baseURL, apiKey, model string) (*LLMRouterEmbedder, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("embedder: base URL required")
	}
	if model == "" {
		model = "bge-m3"
	}
	return &LLMRouterEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model,
		http: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (e *LLMRouterEmbedder) Dimension() int { return EmbeddingDimension }

func (e *LLMRouterEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": e.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embeddings: status %d: %s", resp.StatusCode, snippet)
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embeddings decode: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(parsed.Data), len(texts))
	}
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) != EmbeddingDimension {
			return nil, fmt.Errorf("embeddings: dimension %d != pinned %d — mapping and model must agree", len(d.Embedding), EmbeddingDimension)
		}
		out[i] = d.Embedding
	}
	return out, nil
}
