package llmroutermodel

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Tool-call identity contract: a tool call must always carry (id, name, args)
// on BOTH streaming and non-streaming paths. Missing ids are synthesized
// deterministically; a missing name is a hard error — never emit an orphan
// call the client cannot bind a response to.

func buildChatRequest(req *model.LLMRequest, defaultModel string) (chatRequest, error) {
	out := chatRequest{Model: req.Model}
	if out.Model == "" {
		out.Model = defaultModel
	}

	cfg := req.Config
	if cfg != nil {
		if cfg.SystemInstruction != nil {
			if text := contentText(cfg.SystemInstruction); text != "" {
				out.Messages = append(out.Messages, chatMessage{Role: "system", Content: text})
			}
		}
		out.Temperature = cfg.Temperature
		out.TopP = cfg.TopP
		if cfg.MaxOutputTokens > 0 {
			v := cfg.MaxOutputTokens
			out.MaxTokens = &v
		}
		out.Stop = cfg.StopSequences
		if cfg.ResponseMIMEType == "application/json" {
			out.ResponseFormat = &responseFormat{Type: "json_object"}
		}
		for _, t := range cfg.Tools {
			for _, fd := range t.FunctionDeclarations {
				params, err := declarationParameters(fd)
				if err != nil {
					return chatRequest{}, err
				}
				out.Tools = append(out.Tools, chatTool{
					Type: "function",
					Function: toolDefJSON{
						Name:        fd.Name,
						Description: fd.Description,
						Parameters:  params,
					},
				})
			}
		}
	}

	for _, content := range req.Contents {
		msgs, err := contentToMessages(content)
		if err != nil {
			return chatRequest{}, err
		}
		out.Messages = append(out.Messages, msgs...)
	}
	return out, nil
}

func contentText(c *genai.Content) string {
	var parts []string
	for _, p := range c.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func contentToMessages(c *genai.Content) ([]chatMessage, error) {
	role := "user"
	if c.Role == "model" {
		role = "assistant"
	}

	var text []string
	var calls []toolCall
	var toolResults []chatMessage

	for i, p := range c.Parts {
		switch {
		case p.FunctionCall != nil:
			fc := p.FunctionCall
			if fc.Name == "" {
				return nil, fmt.Errorf("llmroutermodel: function call without name (index %d)", i)
			}
			args, err := json.Marshal(fc.Args)
			if err != nil {
				return nil, fmt.Errorf("llmroutermodel: encode args for %s: %w", fc.Name, err)
			}
			calls = append(calls, toolCall{
				ID:       callID(fc.ID, fc.Name, i),
				Type:     "function",
				Function: functionCall{Name: fc.Name, Arguments: string(args)},
			})
		case p.FunctionResponse != nil:
			fr := p.FunctionResponse
			payload, err := json.Marshal(fr.Response)
			if err != nil {
				return nil, fmt.Errorf("llmroutermodel: encode tool response for %s: %w", fr.Name, err)
			}
			toolResults = append(toolResults, chatMessage{
				Role:       "tool",
				ToolCallID: callID(fr.ID, fr.Name, i),
				Name:       fr.Name,
				Content:    string(payload),
			})
		case p.Text != "":
			text = append(text, p.Text)
		}
	}

	var out []chatMessage
	if len(text) > 0 || len(calls) > 0 {
		out = append(out, chatMessage{
			Role:      role,
			Content:   strings.Join(text, "\n"),
			ToolCalls: calls,
		})
	}
	out = append(out, toolResults...)
	return out, nil
}

// callID keeps tool-call identity stable across both directions: prefer the
// upstream id, otherwise derive a deterministic one from name+position.
func callID(id, name string, index int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("call_%s_%d", name, index)
}

func declarationParameters(fd *genai.FunctionDeclaration) (map[string]any, error) {
	if fd.Parameters != nil {
		return schemaToMap(fd.Parameters), nil
	}
	if fd.ParametersJsonSchema != nil {
		if m, ok := fd.ParametersJsonSchema.(map[string]any); ok {
			return m, nil
		}
		raw, err := json.Marshal(fd.ParametersJsonSchema)
		if err != nil {
			return nil, fmt.Errorf("llmroutermodel: tool %s parameters: %w", fd.Name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("llmroutermodel: tool %s parameters: %w", fd.Name, err)
		}
		return m, nil
	}
	return map[string]any{"type": "object"}, nil
}

func schemaToMap(s *genai.Schema) map[string]any {
	if s == nil {
		return nil
	}
	m := map[string]any{}
	if s.Type != genai.TypeUnspecified {
		m["type"] = strings.ToLower(string(s.Type))
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if s.Format != "" {
		m["format"] = s.Format
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for name, sub := range s.Properties {
			props[name] = schemaToMap(sub)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.Items != nil {
		m["items"] = schemaToMap(s.Items)
	}
	return m
}

func finishReason(reason string) genai.FinishReason {
	switch reason {
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	case "stop", "tool_calls", "":
		return genai.FinishReasonStop
	default:
		return genai.FinishReasonOther
	}
}

func usageMetadata(u *chatUsage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     u.PromptTokens,
		CandidatesTokenCount: u.CompletionTokens,
		TotalTokenCount:      u.TotalTokens,
	}
}

func messageToParts(msg *chatMessage) ([]*genai.Part, error) {
	var parts []*genai.Part
	if msg.Content != "" {
		parts = append(parts, genai.NewPartFromText(msg.Content))
	}
	for i, tc := range msg.ToolCalls {
		if tc.Function.Name == "" {
			return nil, fmt.Errorf("llmroutermodel: upstream tool call missing name (index %d)", i)
		}
		args := map[string]any{}
		if raw := strings.TrimSpace(tc.Function.Arguments); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return nil, fmt.Errorf("llmroutermodel: tool call %s arguments not valid JSON: %w", tc.Function.Name, err)
			}
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID:   callID(tc.ID, tc.Function.Name, i),
			Name: tc.Function.Name,
			Args: args,
		}})
	}
	return parts, nil
}

func responseToLLM(resp *chatResponse) (*model.LLMResponse, error) {
	choice := resp.Choices[0]
	if choice.Message == nil {
		return nil, fmt.Errorf("llmroutermodel: choice has no message")
	}
	parts, err := messageToParts(choice.Message)
	if err != nil {
		return nil, err
	}
	return &model.LLMResponse{
		Content:       &genai.Content{Role: "model", Parts: parts},
		UsageMetadata: usageMetadata(resp.Usage),
		ModelVersion:  resp.Model,
		FinishReason:  finishReason(choice.FinishReason),
		TurnComplete:  true,
	}, nil
}

// --- streaming accumulation -------------------------------------------------

type pendingCall struct {
	id   string
	name string
	args strings.Builder
}

type streamAccumulator struct {
	text     strings.Builder
	calls    []*pendingCall
	byIndex  map[int]*pendingCall
	usage    *chatUsage
	model    string
	finish   string
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{byIndex: map[int]*pendingCall{}}
}

// add consumes one SSE chunk and returns the plain-text delta (if any) so the
// caller can emit partial responses.
func (a *streamAccumulator) add(chunk *chatResponse) (string, error) {
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		a.finish = choice.FinishReason
	}
	delta := choice.Delta
	if delta == nil {
		return "", nil
	}
	if delta.Content != "" {
		a.text.WriteString(delta.Content)
	}
	for i, tc := range delta.ToolCalls {
		idx := i
		if tc.Index != nil {
			idx = *tc.Index
		}
		pc, ok := a.byIndex[idx]
		if !ok {
			pc = &pendingCall{}
			a.byIndex[idx] = pc
			a.calls = append(a.calls, pc)
		}
		if tc.ID != "" {
			pc.id = tc.ID
		}
		if tc.Function.Name != "" {
			pc.name = tc.Function.Name
		}
		pc.args.WriteString(tc.Function.Arguments)
	}
	return delta.Content, nil
}

func (a *streamAccumulator) final() (*model.LLMResponse, error) {
	var parts []*genai.Part
	if a.text.Len() > 0 {
		parts = append(parts, genai.NewPartFromText(a.text.String()))
	}
	for i, pc := range a.calls {
		if pc.name == "" {
			return nil, fmt.Errorf("llmroutermodel: streamed tool call %d never received a name", i)
		}
		args := map[string]any{}
		if raw := strings.TrimSpace(pc.args.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return nil, fmt.Errorf("llmroutermodel: streamed tool call %s arguments not valid JSON: %w", pc.name, err)
			}
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID:   callID(pc.id, pc.name, i),
			Name: pc.name,
			Args: args,
		}})
	}
	return &model.LLMResponse{
		Content:       &genai.Content{Role: "model", Parts: parts},
		UsageMetadata: usageMetadata(a.usage),
		ModelVersion:  a.model,
		FinishReason:  finishReason(a.finish),
		TurnComplete:  true,
	}, nil
}
