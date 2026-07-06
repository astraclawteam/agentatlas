package agent

import (
	"context"
	"encoding/json"
	"fmt"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// ToolCall records one function-tool invocation observed during an agent run:
// the model-issued call and (once the tool executed) its result.
type ToolCall struct {
	ID     string          `json:"id,omitempty"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// RunResult is the outcome of one Knowledge Agent turn: the final assistant
// text plus the trace of every tool call the loop executed.
type RunResult struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// maxRunEvents bounds one turn. Each tool round-trip is ~2 events (the model's
// function call + the tool response), so 64 allows ~30 tool calls — far beyond
// any sane Knowledge Agent turn. Past it we fail loud instead of looping.
const maxRunEvents = 64

// Runner drives the ADK Knowledge Agent loop: model plans, function tools run,
// results feed back, until the model emits its final text. It is the single
// runtime entry point for the agent (NewKnowledgeAgent stays the single
// construction site).
type Runner struct {
	adk *runner.Runner
}

// NewRunner builds the Knowledge Agent on the given model and wraps it in an
// ADK runner with in-memory sessions.
func NewRunner(llm model.LLM) (*Runner, error) {
	ag, err := NewKnowledgeAgent(llm)
	if err != nil {
		return nil, fmt.Errorf("construct knowledge agent: %w", err)
	}
	adkRunner, err := runner.New(runner.Config{
		AppName:           "agentatlas",
		Agent:             ag,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("construct adk runner: %w", err)
	}
	return &Runner{adk: adkRunner}, nil
}

// Run executes one agent turn for (userID, sessionID). Reusing the same ids
// continues the conversation; new ids start fresh (AutoCreateSession).
func (r *Runner) Run(ctx context.Context, userID, sessionID, message string) (RunResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var out RunResult
	// FunctionResponse events carry the call ID; index pending calls both by ID
	// and, for models that omit IDs, by name (first unresolved wins).
	byID := map[string]int{}
	unresolvedByName := map[string][]int{}
	events := 0

	for ev, err := range r.adk.Run(ctx, userID, sessionID, genai.NewContentFromText(message, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			return RunResult{}, fmt.Errorf("agent run: %w", err)
		}
		events++
		if events > maxRunEvents {
			return RunResult{}, fmt.Errorf("agent run exceeded %d events without a final response (model tool loop?)", maxRunEvents)
		}
		if ev.Content == nil {
			continue
		}
		sawToolPart := false
		text := ""
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				sawToolPart = true
				args, mErr := json.Marshal(p.FunctionCall.Args)
				if mErr != nil {
					return RunResult{}, fmt.Errorf("marshal tool args for %s: %w", p.FunctionCall.Name, mErr)
				}
				idx := len(out.ToolCalls)
				out.ToolCalls = append(out.ToolCalls, ToolCall{
					ID:   p.FunctionCall.ID,
					Name: p.FunctionCall.Name,
					Args: args,
				})
				if p.FunctionCall.ID != "" {
					byID[p.FunctionCall.ID] = idx
				}
				unresolvedByName[p.FunctionCall.Name] = append(unresolvedByName[p.FunctionCall.Name], idx)
			case p.FunctionResponse != nil:
				sawToolPart = true
				result, mErr := json.Marshal(p.FunctionResponse.Response)
				if mErr != nil {
					return RunResult{}, fmt.Errorf("marshal tool result for %s: %w", p.FunctionResponse.Name, mErr)
				}
				if idx, ok := byID[p.FunctionResponse.ID]; ok && p.FunctionResponse.ID != "" {
					out.ToolCalls[idx].Result = result
					unresolvedByName[p.FunctionResponse.Name] = dropIndex(unresolvedByName[p.FunctionResponse.Name], idx)
				} else if pendings := unresolvedByName[p.FunctionResponse.Name]; len(pendings) > 0 {
					out.ToolCalls[pendings[0]].Result = result
					unresolvedByName[p.FunctionResponse.Name] = pendings[1:]
				}
			case p.Text != "" && !p.Thought:
				text += p.Text
			}
		}
		// The final assistant message is a model event with text and no tool
		// parts; intermediate reasoning text before a tool call is superseded.
		if text != "" && !sawToolPart {
			out.Text = text
		}
	}

	if out.Text == "" && len(out.ToolCalls) == 0 {
		return RunResult{}, fmt.Errorf("agent run produced neither text nor tool calls")
	}
	return out, nil
}

func dropIndex(s []int, idx int) []int {
	for i, v := range s {
		if v == idx {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
