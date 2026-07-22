// Package llmroutermodel implements the adk-llmrouter-model adapter: an ADK
// model.LLM backed by any OpenAI-compatible Chat Completions endpoint.
// llmrouter is the default and recommended deployment; nothing here is
// vendor-specific.
package llmroutermodel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

type Config struct {
	// BaseURL of the OpenAI-compatible endpoint, e.g. http://llmrouter:8000/v1
	BaseURL string
	APIKey  string
	// DefaultModel is used when the ADK request does not carry a model name.
	DefaultModel string
	Timeout      time.Duration
	// TLS configures the llmrouter link's transport security
	// (services/agentatlas/internal/transportsecurity); nil, or a Manager
	// built with LinkConfig.Mode == ModeOff, keeps today's plaintext
	// behavior.
	TLS *transportsecurity.Manager
}

type Client struct {
	cfg  Config
	http *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("llmroutermodel: BaseURL is required")
	}
	if cfg.DefaultModel == "" {
		return nil, fmt.Errorf("llmroutermodel: DefaultModel is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	transport := &http.Transport{}
	if cfg.TLS != nil {
		if err := cfg.TLS.ConfigureTransport(transport); err != nil {
			return nil, fmt.Errorf("llmroutermodel: tls: %w", err)
		}
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout, Transport: transport}}, nil
}

// --- OpenAI-compatible wire types -----------------------------------------

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function toolDefJSON `json:"function"`
}

type toolDefJSON struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Tools          []chatTool      `json:"tools,omitempty"`
	Temperature    *float32        `json:"temperature,omitempty"`
	TopP           *float32        `json:"top_p,omitempty"`
	MaxTokens      *int32          `json:"max_tokens,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

type chatUsage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
	Error   *apiError    `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// --- transport --------------------------------------------------------------

func (c *Client) do(ctx context.Context, req chatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llmroutermodel: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llmroutermodel: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llmroutermodel: status %d: %s", resp.StatusCode, snippet)
	}
	return resp, nil
}

// probe performs the cheapest authenticated round trip an OpenAI-compatible
// endpoint offers: listing the model shelf. See Model.Probe.
func (c *Client) probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.BaseURL, "/")+"/models", nil)
	if err != nil {
		return err
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("llmroutermodel: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// No body snippet: this error reaches an operator log from a readiness
		// loop that runs forever, and a router error body can echo request
		// headers.
		return fmt.Errorf("llmroutermodel: model list returned status %d", resp.StatusCode)
	}
	return nil
}

// Complete performs a non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req chatRequest) (*chatResponse, error) {
	req.Stream = false
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("llmroutermodel: decode response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("llmroutermodel: api error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llmroutermodel: empty choices")
	}
	return &out, nil
}

// Stream performs a streaming chat completion, invoking onChunk per SSE chunk
// until [DONE].
func (c *Client) Stream(ctx context.Context, req chatRequest, onChunk func(*chatResponse) error) error {
	req.Stream = true
	req.StreamOptions = &streamOptions{IncludeUsage: true}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return nil
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("llmroutermodel: decode chunk: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("llmroutermodel: api error: %s", chunk.Error.Message)
		}
		if err := onChunk(&chunk); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("llmroutermodel: stream: %w", err)
	}
	return nil
}
