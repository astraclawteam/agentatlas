package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	agentapi "github.com/astraclawteam/agentatlas/services/agentatlas/api/openapi/gen/agent"
	runtimeapi "github.com/astraclawteam/agentatlas/services/agentatlas/api/openapi/gen/runtime"
)

func nestedMap(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, key := range path {
		value, ok := current[key]
		if !ok {
			t.Fatalf("OpenAPI missing %s", key)
		}
		current, ok = value.(map[string]any)
		if !ok {
			t.Fatalf("OpenAPI %s is %T, want object", key, value)
		}
	}
	return current
}

func namedSchema(t *testing.T, schemas map[string]any, name string) map[string]any {
	t.Helper()
	value, ok := schemas[name]
	if !ok {
		t.Fatalf("contract missing schema %s", name)
	}
	schema, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("schema %s is %T, want object", name, value)
	}
	assertType(t, schema, "object")
	return schema
}

func assertObjectProperties(t *testing.T, schema map[string]any, required, optional []string) {
	t.Helper()
	properties := nestedMap(t, schema, "properties")
	wantProperties := append(append([]string(nil), required...), optional...)
	sort.Strings(wantProperties)
	gotProperties := make([]string, 0, len(properties))
	for name := range properties {
		gotProperties = append(gotProperties, name)
	}
	sort.Strings(gotProperties)
	if !reflect.DeepEqual(gotProperties, wantProperties) {
		t.Fatalf("properties = %v, want %v", gotProperties, wantProperties)
	}

	gotRequired, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("required is %T, want array", schema["required"])
	}
	gotRequiredNames := make([]string, 0, len(gotRequired))
	for _, value := range gotRequired {
		name, ok := value.(string)
		if !ok {
			t.Fatalf("required value is %T, want string", value)
		}
		gotRequiredNames = append(gotRequiredNames, name)
	}
	sort.Strings(gotRequiredNames)
	wantRequired := append([]string(nil), required...)
	sort.Strings(wantRequired)
	if !reflect.DeepEqual(gotRequiredNames, wantRequired) {
		t.Fatalf("required = %v, want %v", gotRequiredNames, wantRequired)
	}
}

func property(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	properties := nestedMap(t, schema, "properties")
	value, ok := properties[name]
	if !ok {
		t.Fatalf("property missing %s", name)
	}
	propertySchema, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("property %s is %T, want object", name, value)
	}
	return propertySchema
}

func assertPropertyType(t *testing.T, schema map[string]any, name, want string) {
	t.Helper()
	assertType(t, property(t, schema, name), want)
}

func assertType(t *testing.T, schema map[string]any, want string) {
	t.Helper()
	if got := schema["type"]; got != want {
		t.Fatalf("type = %v, want %s", got, want)
	}
}

func assertStringArray(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	array := property(t, schema, name)
	assertType(t, array, "array")
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("property %s items is %T, want object", name, array["items"])
	}
	assertType(t, items, "string")
}

func assertObjectArray(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	array := property(t, schema, name)
	assertType(t, array, "array")
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("property %s items is %T, want object", name, array["items"])
	}
	assertType(t, items, "object")
}

func assertRefArray(t *testing.T, schema map[string]any, name, want string) {
	t.Helper()
	array := property(t, schema, name)
	assertType(t, array, "array")
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("property %s items is %T, want object", name, array["items"])
	}
	assertRef(t, items, want)
}

func assertOpenObject(t *testing.T, schema map[string]any) {
	t.Helper()
	assertType(t, schema, "object")
	if got := schema["additionalProperties"]; got != true {
		t.Fatalf("additionalProperties = %v, want true", got)
	}
}

func assertDateTime(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	dateTime := property(t, schema, name)
	assertType(t, dateTime, "string")
	if got := dateTime["format"]; got != "date-time" {
		t.Fatalf("property %s format = %v, want date-time", name, got)
	}
}

func assertPositiveInteger(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	integer := property(t, schema, name)
	assertType(t, integer, "integer")
	if got := integer["minimum"]; got != 1 {
		t.Fatalf("property %s minimum = %v, want 1", name, got)
	}
}

func assertNonNegativeInteger(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	integer := property(t, schema, name)
	assertType(t, integer, "integer")
	if got := integer["minimum"]; got != 0 {
		t.Fatalf("property %s minimum = %v, want 0", name, got)
	}
}

func assertArrayItemEnum(t *testing.T, schema map[string]any, name string, want []any) {
	t.Helper()
	array := property(t, schema, name)
	assertType(t, array, "array")
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("property %s items is %T, want object", name, array["items"])
	}
	assertEnum(t, items, want)
}

func assertRef(t *testing.T, schema map[string]any, want string) {
	t.Helper()
	if got := schema["$ref"]; got != want {
		t.Fatalf("$ref = %v, want %s", got, want)
	}
}

func assertEnum(t *testing.T, schema map[string]any, want []any) {
	t.Helper()
	got, ok := schema["enum"].([]any)
	if !ok {
		t.Fatalf("enum is %T, want array", schema["enum"])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enum = %v, want %v", got, want)
	}
}

// Generated types are part of the public contract surface — referencing them
// here makes the committed gen packages a compile-time dependency of the
// suite, so "spec regenerated but not committed" breaks the build.
var (
	_ = runtimeapi.AnswerTrace{}
	_ = runtimeapi.WorkBriefIngest{}
	_ = runtimeapi.RetrievalPlanRequest{}
	_ = agentapi.CreateWorkflowDraftJSONBody{}
	_ = agentapi.StartWorkflowRunJSONBody{}
	_ = agentapi.WorkflowRef{}
	_ = agentapi.DreamPolicyDefinition{}
	_ = agentapi.DreamRunView{}
	_ = agentapi.DreamSummaryView{}
	_ = agentapi.StructuredSignal{}
	_ = agentapi.Coverage{}
	_ = agentapi.MissingInput{}
	_ = agentapi.ChangeDraft{}
	_ = agentapi.RiskAssessment{}
	_ = agentapi.ReviewRoute{}
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
