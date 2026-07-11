package schemas

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/governance"
	"gopkg.in/yaml.v3"
)

func assertContractParity(t *testing.T, schemaJSON []byte, openAPIPath string, names map[string]string) {
	t.Helper()
	var jsonDoc map[string]any
	if err := json.Unmarshal(schemaJSON, &jsonDoc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var openAPIDoc map[string]any
	if err := yaml.Unmarshal(raw, &openAPIDoc); err != nil {
		t.Fatal(err)
	}
	defs := jsonDoc["$defs"].(map[string]any)
	components := openAPIDoc["components"].(map[string]any)["schemas"].(map[string]any)
	for jsonName, openAPIName := range names {
		t.Run(jsonName, func(t *testing.T) {
			got := normalizedContractShape(defs[jsonName].(map[string]any), defs, "#/$defs/")
			want := normalizedContractShape(components[openAPIName].(map[string]any), components, "#/components/schemas/")
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("JSON Schema/OpenAPI drift\nJSON: %#v\nOpenAPI: %#v", got, want)
			}
		})
	}
}

func normalizedContractShape(schema, root map[string]any, prefix string) map[string]any {
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, prefix) {
		schema = root[strings.TrimPrefix(ref, prefix)].(map[string]any)
	}
	out := map[string]any{}
	for _, key := range []string{"type", "format", "pattern", "enum", "default", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "maxProperties", "uniqueItems", "additionalProperties"} {
		if value, ok := schema[key]; ok {
			out[key] = value
		}
	}
	if required, ok := schema["required"].([]any); ok {
		values := make([]string, len(required))
		for i, value := range required {
			values[i] = value.(string)
		}
		sort.Strings(values)
		out["required"] = values
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		values := map[string]any{}
		for name, value := range properties {
			values[name] = normalizedContractShape(value.(map[string]any), root, prefix)
		}
		out["properties"] = values
	}
	if items, ok := schema["items"].(map[string]any); ok {
		out["items"] = normalizedContractShape(items, root, prefix)
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		if _, hasMode := properties["mode"]; hasMode {
			if branches, ok := schema["oneOf"].([]any); ok {
				values := make([]any, len(branches))
				for i, branch := range branches {
					values[i] = normalizedBranch(branch.(map[string]any))
				}
				out["oneOf"] = values
			}
		}
	}
	return out
}

func normalizedBranch(schema map[string]any) map[string]any {
	out := map[string]any{}
	if value, ok := schema["const"]; ok {
		out["enum"] = []any{value}
	}
	for _, key := range []string{"enum", "minItems", "minProperties"} {
		if value, ok := schema[key]; ok {
			out[key] = value
		}
	}
	if required, ok := schema["required"].([]any); ok {
		values := append([]any(nil), required...)
		sort.Slice(values, func(i, j int) bool { return values[i].(string) < values[j].(string) })
		out["required"] = values
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		values := map[string]any{}
		for name, value := range properties {
			values[name] = normalizedBranch(value.(map[string]any))
		}
		out["properties"] = values
	}
	if value, ok := schema["not"].(map[string]any); ok {
		out["not"] = normalizedBranch(value)
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if branches, ok := schema[key].([]any); ok {
			values := make([]any, len(branches))
			for i, branch := range branches {
				values[i] = normalizedBranch(branch.(map[string]any))
			}
			out[key] = values
		}
	}
	return out
}

func assertSDKShape(t *testing.T, schemaJSON []byte, name string, typ reflect.Type) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	schema := doc["$defs"].(map[string]any)[name].(map[string]any)
	properties := schema["properties"].(map[string]any)
	required := map[string]bool{}
	for _, value := range schema["required"].([]any) {
		required[value.(string)] = true
	}
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		parts := strings.Split(typ.Field(i).Tag.Get("json"), ",")
		name := parts[0]
		seen[name] = true
		optional := len(parts) > 1 && parts[1] == "omitempty"
		if required[name] == optional {
			t.Fatalf("SDK %s required/omitempty drift for %s", typ.Name(), name)
		}
	}
	if len(seen) != len(properties) {
		t.Fatalf("SDK %s field count %d != schema properties %d", typ.Name(), len(seen), len(properties))
	}
	for name := range properties {
		if !seen[name] {
			t.Fatalf("SDK %s missing JSON field %s", typ.Name(), name)
		}
	}
}

func TestJSONSchemaOpenAPIAndSDKContractParity(t *testing.T) {
	assertContractParity(t, DreamSchemaJSON, "../api/openapi/atlas-agent.yaml", map[string]string{
		"WorkflowRef": "WorkflowRef", "StructuredSignal": "StructuredSignal", "Coverage": "Coverage", "MissingInput": "MissingInput",
		"DreamPolicyDefinition": "DreamPolicyDefinition", "DreamSummaryView": "DreamSummaryView", "DreamRunView": "DreamRunView",
	})
	assertContractParity(t, GovernanceSchemaJSON, "../api/openapi/atlas-agent.yaml", map[string]string{
		"ChangeDraft": "ChangeDraft", "RiskAssessment": "RiskAssessment", "ReviewRoute": "ReviewRoute",
	})
	assertSDKShape(t, DreamSchemaJSON, "DreamPolicyDefinition", reflect.TypeOf(sdkdream.DreamPolicyDefinition{}))
	assertSDKShape(t, DreamSchemaJSON, "DreamSummaryView", reflect.TypeOf(sdkdream.DreamSummaryView{}))
	assertSDKShape(t, DreamSchemaJSON, "DreamRunView", reflect.TypeOf(sdkdream.DreamRunView{}))
	assertSDKShape(t, GovernanceSchemaJSON, "ChangeDraft", reflect.TypeOf(governance.ChangeDraft{}))
	assertSDKShape(t, GovernanceSchemaJSON, "RiskAssessment", reflect.TypeOf(governance.RiskAssessment{}))
	assertSDKShape(t, GovernanceSchemaJSON, "ReviewRoute", reflect.TypeOf(governance.ReviewRoute{}))

	_ = sdkdream.DreamPolicyDefinition{}
	_ = sdkdream.DreamRunView{}
	_ = governance.ChangeDraft{}
	_ = governance.RiskAssessment{}
	_ = governance.ReviewRoute{}
	if err := (governance.RiskAssessment{RiskLevel: governance.RiskHigh, RiskReasons: []string{"binding changed"}}).Validate(); err != nil {
		t.Fatalf("SDK valid risk rejected: %v", err)
	}
}
