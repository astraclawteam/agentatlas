// Package llmutil is the shared plumbing for one-shot LLM calls over the ADK
// model.LLM contract: a non-streaming text completion and strict-ish JSON
// extraction from model replies. Higher-level prompting stays in the callers.
package llmutil

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// CompleteText runs one non-streaming completion and returns the concatenated
// non-thought text. Empty model output is an error (fail loud).
func CompleteText(ctx context.Context, llm model.LLM, system, user string) (string, error) {
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText(system)}},
		},
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{genai.NewPartFromText(user)}},
		},
	}
	var text strings.Builder
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", err
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if !p.Thought {
					text.WriteString(p.Text)
				}
			}
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", fmt.Errorf("model returned empty text")
	}
	return text.String(), nil
}

// ParseJSON extracts the first JSON object from a model reply (tolerating
// markdown fences or stray prose around it) and unmarshals it.
func ParseJSON(raw string, into any) error {
	t := strings.TrimSpace(raw)
	i := strings.Index(t, "{")
	j := strings.LastIndex(t, "}")
	if i < 0 || j <= i {
		return fmt.Errorf("no JSON object found")
	}
	return json.Unmarshal([]byte(t[i:j+1]), into)
}
