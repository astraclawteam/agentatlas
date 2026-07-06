package llmroutermodel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DescribeImage on the adapter delegates to the underlying client.
func (m *Model) DescribeImage(ctx context.Context, model, prompt string, imageData []byte, mimeType string) (string, error) {
	return m.client.DescribeImage(ctx, model, prompt, imageData, mimeType)
}

// DescribeImage runs one multimodal completion — prompt + inline image (data
// URL content part) — through the OpenAI-compatible chat route. llmrouter
// capability-routes VLM-able models (e.g. doubao-seed vision tier); pass the
// deployment's vision model explicitly, or empty to use the default model.
func (c *Client) DescribeImage(ctx context.Context, model, prompt string, imageData []byte, mimeType string) (string, error) {
	if len(imageData) == 0 {
		return "", fmt.Errorf("llmroutermodel: image data required")
	}
	if model == "" {
		model = c.cfg.DefaultModel
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(imageData)
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": prompt},
				{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			},
		}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vlm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("vlm: status %d: %s", resp.StatusCode, snippet)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("vlm decode: %w", err)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("vlm returned no description")
	}
	return parsed.Choices[0].Message.Content, nil
}
