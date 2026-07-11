package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadAgentContract(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi", "atlas-agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestGeneratedAgentTypesMatchFreshOAPICodegen(t *testing.T) {
	serviceRoot := filepath.Join("..", "..")
	configPath := filepath.Join(serviceRoot, "api", "openapi", "oapi-codegen-agent.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.ToSlash(filepath.Join(t.TempDir(), "types.gen.go"))
	tempConfig := filepath.Join(t.TempDir(), "oapi-codegen-agent.yaml")
	rewritten := strings.Replace(string(config), "output: api/openapi/gen/agent/types.gen.go", "output: "+output, 1)
	if rewritten == string(config) {
		t.Fatal("generator config output line changed; drift test cannot redirect output")
	}
	if err := os.WriteFile(tempConfig, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	generate := func() []byte {
		t.Helper()
		cmd := exec.Command("go", "tool", "oapi-codegen", "-config", tempConfig, "api/openapi/atlas-agent.yaml")
		cmd.Dir = serviceRoot
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("oapi-codegen v2.7.1: %v\n%s", err, combined)
		}
		generated, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return generated
	}
	first := generate()
	second := generate()
	if !bytes.Equal(first, second) {
		t.Fatal("two fresh oapi-codegen v2.7.1 runs were not byte-stable")
	}
	committed, err := os.ReadFile(filepath.Join(serviceRoot, "api", "openapi", "gen", "agent", "types.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, committed) {
		t.Fatal("committed agent types differ from a fresh oapi-codegen v2.7.1 run")
	}
}

func TestCanonicalDreamAndGovernanceNamesReplaceLegacyComponents(t *testing.T) {
	doc := loadAgentContract(t)
	schemas := nestedMap(t, doc, "components", "schemas")
	for _, name := range []string{
		"WorkflowRef", "StructuredSignal", "Coverage", "MissingInput",
		"DreamPolicyDefinition", "DreamSummaryView", "DreamRunView",
		"ChangeDraft", "RiskAssessment", "ReviewRoute",
	} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("canonical component missing %s", name)
		}
	}
	for _, legacy := range []string{"DreamPolicyDraft", "DreamPolicy", "DreamSummary", "DreamRun", "PublishDecision"} {
		if _, ok := schemas[legacy]; ok {
			t.Errorf("legacy component remains public: %s", legacy)
		}
	}
}

func TestDreamPolicyRouteConsumesCanonicalDefinition(t *testing.T) {
	doc := loadAgentContract(t)
	post := nestedMap(t, doc, "paths", "/v1/dream-policies", "post")
	request := nestedMap(t, post, "requestBody", "content", "application/json", "schema")
	if got := request["$ref"]; got != "#/components/schemas/DreamPolicyDefinition" {
		t.Fatalf("create request ref = %v", got)
	}
}

func TestDreamRunViewMatchesCanonicalRuntimeFields(t *testing.T) {
	doc := loadAgentContract(t)
	schema := namedSchema(t, nestedMap(t, doc, "components", "schemas"), "DreamRunView")
	for _, field := range []string{
		"run_id", "status", "org_unit_id", "window_start", "window_end", "policy_version",
		"workflow", "parent_run_ids", "input_count", "coverage", "missing_inputs", "facts",
		"themes", "trends", "risks", "todos", "display_summary", "evidence_pointer_id", "rerun_of_run_id",
	} {
		if _, ok := nestedMap(t, schema, "properties")[field]; !ok {
			t.Errorf("DreamRunView missing %s", field)
		}
	}
	assertRef(t, property(t, schema, "workflow"), "#/components/schemas/WorkflowRef")
	assertRef(t, property(t, schema, "coverage"), "#/components/schemas/Coverage")
	assertRefArray(t, schema, "missing_inputs", "#/components/schemas/MissingInput")
	for _, field := range []string{"facts", "themes", "trends", "risks", "todos"} {
		assertRefArray(t, schema, field, "#/components/schemas/StructuredSignal")
	}
	assertEnum(t, property(t, schema, "status"), []any{"pending", "running", "waiting_confirmation", "succeeded", "failed"})
}

func TestGovernanceEnumsAndConditionalRoutesAreExplicit(t *testing.T) {
	doc := loadAgentContract(t)
	schemas := nestedMap(t, doc, "components", "schemas")
	risk := namedSchema(t, schemas, "RiskAssessment")
	assertEnum(t, property(t, risk, "risk_level"), []any{"low", "high"})
	route := namedSchema(t, schemas, "ReviewRoute")
	if oneOf, ok := route["oneOf"].([]any); !ok || len(oneOf) != 3 {
		t.Fatalf("ReviewRoute oneOf = %#v, want three mode branches", route["oneOf"])
	}
	for _, field := range []string{"change_id", "resource_type", "resource_id", "requester_user_id", "risk_level", "mode", "state", "org_path"} {
		if _, ok := nestedMap(t, route, "properties")[field]; !ok {
			t.Errorf("ReviewRoute missing binding %s", field)
		}
	}
}
