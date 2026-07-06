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

// Reranker reorders candidates by relevance. Production routes through
// llmrouter's rerank capability (Cohere-style /rerank contract).
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]RankedItem, error)
}

type RankedItem struct {
	Index int
	Score float64
}

type LLMRouterReranker struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

var _ Reranker = (*LLMRouterReranker)(nil)

func NewLLMRouterReranker(baseURL, apiKey, model string) (*LLMRouterReranker, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("reranker: base URL required")
	}
	if model == "" {
		model = "bge-reranker"
	}
	return &LLMRouterReranker{
		baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model,
		http: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (r *LLMRouterReranker) Rerank(ctx context.Context, query string, documents []string, topN int) ([]RankedItem, error) {
	body, err := json.Marshal(map[string]any{
		"model": r.model, "query": query, "documents": documents, "top_n": topN,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank: status %d: %s", resp.StatusCode, snippet)
	}
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("rerank decode: %w", err)
	}
	out := make([]RankedItem, 0, len(parsed.Results))
	for _, item := range parsed.Results {
		out = append(out, RankedItem{Index: item.Index, Score: item.RelevanceScore})
	}
	return out, nil
}
