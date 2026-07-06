package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	agentapi "github.com/astraclawteam/agentatlas/services/agentatlas/api/openapi/gen/agent"
	runtimeapi "github.com/astraclawteam/agentatlas/services/agentatlas/api/openapi/gen/runtime"
)

// Generated types are part of the public contract surface — referencing them
// here makes the committed gen packages a compile-time dependency of the
// suite, so "spec regenerated but not committed" breaks the build.
var (
	_ = runtimeapi.AnswerTrace{}
	_ = runtimeapi.WorkBriefIngest{}
	_ = runtimeapi.RetrievalPlanRequest{}
	_ = agentapi.DreamPolicyDraft{}
	_ = agentapi.CreateWorkflowDraftJSONBody{}
	_ = agentapi.StartWorkflowRunJSONBody{}
)

func specPaths(t *testing.T, file string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi", file))
	if err != nil {
		t.Fatalf("read spec %s: %v", file, err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec %s: %v", file, err)
	}
	out := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				out[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return out
}

func servedRoutes(t *testing.T, mux *chi.Mux) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if route == "" || route == "/healthz" || route == "/metrics" {
			return nil
		}
		out[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSets(a, b map[string]bool) []string {
	var missing []string
	for k := range a {
		if !b[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// TestContractDrift fails when the served chi routes and the hand-authored
// OpenAPI specs diverge in either direction — the "spec exists but is
// ignored" state can no longer happen silently.
func TestContractDrift(t *testing.T) {
	runtimeSpec := specPaths(t, "atlas-runtime.yaml")
	apiRoutes := servedRoutes(t, NewRouter(RouterDeps{Nexus: adminMock()}))
	if m := diffSets(apiRoutes, runtimeSpec); len(m) > 0 {
		t.Fatalf("atlas-api serves routes missing from atlas-runtime.yaml: %v", m)
	}
	if m := diffSets(runtimeSpec, apiRoutes); len(m) > 0 {
		t.Fatalf("atlas-runtime.yaml declares routes atlas-api does not serve: %v", m)
	}

	agentSpec := specPaths(t, "atlas-agent.yaml")
	agentRoutes := servedRoutes(t, NewAgentRouter(AgentRouterDeps{Nexus: adminMock()}))
	if m := diffSets(agentRoutes, agentSpec); len(m) > 0 {
		t.Fatalf("atlas-agent serves routes missing from atlas-agent.yaml: %v", m)
	}
	if m := diffSets(agentSpec, agentRoutes); len(m) > 0 {
		t.Fatalf("atlas-agent.yaml declares routes atlas-agent does not serve: %v", m)
	}
}

// TestProtoMirrorsSDK is the locked-decision drift guard: proto codegen is
// deferred (docs/specs/codegen-decision.md), so the hand-authored proto
// messages must keep naming parity with the SDK structs they mirror.
func TestProtoMirrorsSDK(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "proto", "agentatlas", "workflow", "v1", "workflow.proto"))
	if err != nil {
		t.Fatalf("read workflow.proto: %v", err)
	}
	proto := string(raw)
	for _, decl := range []string{
		"message Workflow", "message Node", "message Edge",
		"workflow_id", "risk_level", "requires_confirmation",
	} {
		if !strings.Contains(proto, decl) {
			t.Fatalf("workflow.proto lost parity with sdk/go/workflow: missing %q", decl)
		}
	}
	if !strings.Contains(proto, "WorkflowService") {
		t.Fatal(fmt.Sprintf("workflow.proto missing WorkflowService declaration"))
	}
}
