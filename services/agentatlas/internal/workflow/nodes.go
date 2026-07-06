package workflow

import (
	"context"
	"errors"
	"fmt"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

// ErrExecutorNotWired marks node types whose executors land in later goals
// (parser: Goal 9, retrieval: Goal 10, dream: Goal 11, nexus/answer/trace:
// Goal 12). Hitting one fails the run loudly instead of pretending success.
var ErrExecutorNotWired = errors.New("workflow: executor not wired")

// Executor runs one node. Outputs become visible to downstream nodes via the
// run context.
type Executor interface {
	Execute(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error)
}

// RunContext carries run identity, the run input, and node outputs.
type RunContext struct {
	RunID        string
	EnterpriseID string
	Input        map[string]any
	Outputs      map[string]map[string]any
}

// Registry maps node types to executors. All sixteen built-in types are
// present from Goal 7 on; later goals replace the not-wired entries.
type Registry map[sdkworkflow.NodeType]Executor

func (r Registry) Register(t sdkworkflow.NodeType, e Executor) { r[t] = e }

type executorFunc func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error)

func (f executorFunc) Execute(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
	return f(ctx, node, run)
}

func notWired(t sdkworkflow.NodeType) Executor {
	return executorFunc(func(context.Context, sdkworkflow.Node, *RunContext) (map[string]any, error) {
		return nil, fmt.Errorf("node type %s: %w", t, ErrExecutorNotWired)
	})
}

// NewRegistry returns the built-in registry.
func NewRegistry() Registry {
	r := Registry{}

	// input.manual passes the run input through.
	r.Register(sdkworkflow.NodeInputManual, executorFunc(
		func(_ context.Context, _ sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			return map[string]any{"input": run.Input}, nil
		}))

	// input.evidence_pointer resolves the pointer id from node config or input.
	r.Register(sdkworkflow.NodeInputEvidencePointer, executorFunc(
		func(_ context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if id, ok := node.Config["evidence_pointer_id"].(string); ok && id != "" {
				return map[string]any{"evidence_pointer_id": id}, nil
			}
			if id, ok := run.Input["evidence_pointer_id"].(string); ok && id != "" {
				return map[string]any{"evidence_pointer_id": id}, nil
			}
			return nil, fmt.Errorf("node %s: evidence_pointer_id missing from config and input", node.ID)
		}))

	// human.confirm is handled by the runtime itself (pause point); the
	// registry entry exists so validation knows the type is executable.
	r.Register(sdkworkflow.NodeHumanConfirm, executorFunc(
		func(_ context.Context, node sdkworkflow.Node, _ *RunContext) (map[string]any, error) {
			return nil, fmt.Errorf("node %s: human.confirm must be handled by the runtime pause logic", node.ID)
		}))

	for _, t := range []sdkworkflow.NodeType{
		sdkworkflow.NodeParserDocument, sdkworkflow.NodeParserImage,
		sdkworkflow.NodeParserLongImage, sdkworkflow.NodeParserAudio,
		sdkworkflow.NodeParserVideo, sdkworkflow.NodeTransformExtractSOP,
		sdkworkflow.NodeTransformSummarize, sdkworkflow.NodeRetrievalSearch,
		sdkworkflow.NodeNexusLocate, sdkworkflow.NodeNexusRead,
		sdkworkflow.NodeDreamAggregate, sdkworkflow.NodeAnswerGenerate,
		sdkworkflow.NodeTraceAppend,
	} {
		r.Register(t, notWired(t))
	}
	return r
}
