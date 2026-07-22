package llmroutermodel

import (
	"context"
	"iter"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Model is the adk-llmrouter-model adapter: ADK model.LLM over any
// OpenAI-compatible Chat Completions endpoint.
type Model struct {
	client *Client
}

var _ model.LLM = (*Model)(nil)

func New(cfg Config) (*Model, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Model{client: client}, nil
}

func (m *Model) Name() string {
	return m.client.cfg.DefaultModel
}

// Probe reports whether the configured router answers, for readiness surfaces.
//
// It lists models rather than generating anything: a readiness check must not
// spend tokens or bill a tenant. The API key travels with it, so an endpoint
// that is reachable but rejects this deployment's credential is reported as a
// failure -- which is the case a "is the host up" ping would call healthy.
func (m *Model) Probe(ctx context.Context) error { return m.client.probe(ctx) }

// GenerateContent maps the ADK request to the OpenAI-compatible wire format.
// In streaming mode, text deltas are yielded as Partial responses and the
// final response carries the accumulated content including complete tool
// calls (identity contract: streaming == non-streaming).
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		creq, err := buildChatRequest(req, m.client.cfg.DefaultModel)
		if err != nil {
			yield(nil, err)
			return
		}
		start := time.Now()

		if !stream {
			resp, err := m.client.Complete(ctx, creq)
			if err != nil {
				yield(nil, err)
				return
			}
			out, err := responseToLLM(resp)
			if err != nil {
				yield(nil, err)
				return
			}
			out.CustomMetadata = routeMetadata(resp.Model, creq.Model, start)
			yield(out, nil)
			return
		}

		acc := newStreamAccumulator()
		streamErr := m.client.Stream(ctx, creq, func(chunk *chatResponse) error {
			delta, err := acc.add(chunk)
			if err != nil {
				return err
			}
			if delta != "" {
				partial := &model.LLMResponse{
					Content: &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText(delta)}},
					Partial: true,
				}
				if !yield(partial, nil) {
					return context.Canceled
				}
			}
			return nil
		})
		if streamErr != nil {
			yield(nil, streamErr)
			return
		}
		final, err := acc.final()
		if err != nil {
			yield(nil, err)
			return
		}
		final.CustomMetadata = routeMetadata(acc.model, creq.Model, start)
		yield(final, nil)
	}
}

// routeMetadata feeds Answer Trace model events: requested route, served
// model, and latency.
func routeMetadata(servedModel, requestedModel string, start time.Time) map[string]any {
	if servedModel == "" {
		servedModel = requestedModel
	}
	return map[string]any{
		"model_route":     requestedModel,
		"model_served":    servedModel,
		"latency_ms":      time.Since(start).Milliseconds(),
	}
}
