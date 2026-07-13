// Package workcase_test proves the embedded workcase JSON Schemas compile,
// behave (valid documents pass, boundary-violating documents fail — the
// same accept/reject discipline services/agentatlas/schemas/schemas_test.go
// applies to the workflow/parser/atlasdocument families) and mirror the SDK
// types field-for-field. The parity half re-implements the reflection based
// drift check from services/agentatlas/schemas/contract_parity_test.go
// (assertSDKShape) locally because that helper is unexported and this test
// lives in a different package (services/agentatlas/schemas/workcase), one
// directory below the schemas package that embeds the JSON.
package workcase_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/astraclawteam/agentatlas/sdk/go/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/schemas"
)

// compile proves a schema document is well-formed JSON Schema and returns
// the compiled schema for behavioral accept/reject fixtures — the same bar
// services/agentatlas/schemas/schemas_test.go holds workflow, parser and
// atlasdocument to (there via jsonschema.NewCompiler().Compile(path); here
// via the embedded bytes directly, so the check exercises the exact content
// go:embed wired into services/agentatlas/schemas/embed.go).
func compile(t *testing.T, resource string, schemaJSON []byte) *jsonschema.Schema {
	t.Helper()
	var doc any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatalf("%s: invalid JSON: %v", resource, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(resource, doc); err != nil {
		t.Fatalf("%s: add resource: %v", resource, err)
	}
	sch, err := compiler.Compile(resource)
	if err != nil {
		t.Fatalf("%s: compile: %v", resource, err)
	}
	return sch
}

func mustValidate(t *testing.T, sch *jsonschema.Schema, doc string) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("fixture json: %v", err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func mustReject(t *testing.T, sch *jsonschema.Schema, doc, why string) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("fixture json: %v", err)
	}
	if err := sch.Validate(v); err == nil {
		t.Fatalf("expected rejection (%s), document passed", why)
	}
}

func dereferenceJSONSchema(schema, defs map[string]any) map[string]any {
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
			return def
		}
	}
	return schema
}

func sdkJSONType(typ reflect.Type) string {
	if typ == reflect.TypeOf(time.Time{}) {
		return "string"
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Array, reflect.Slice:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return ""
	}
}

// assertSDKShape proves the embedded JSON Schema definition named `name`
// mirrors the SDK struct `typ` field-for-field: every JSON property has a
// matching struct field with the same JSON type, required and omitempty are
// exact opposites, and no field or property is orphaned on either side. On
// any drift the schema and the SDK have silently diverged and this test
// fails loudly instead of letting the two contracts disagree at runtime.
func assertSDKShape(t *testing.T, schemaJSON []byte, name string, typ reflect.Type) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no $defs")
	}
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("schema has no $defs/%s", name)
	}
	properties, _ := def["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := def["required"].([]any); ok {
		for _, value := range list {
			required[value.(string)] = true
		}
	}
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		parts := strings.Split(typ.Field(i).Tag.Get("json"), ",")
		fieldName := parts[0]
		seen[fieldName] = true
		optional := len(parts) > 1 && parts[1] == "omitempty"
		if required[fieldName] == optional {
			t.Fatalf("SDK %s required/omitempty drift for %s (schema required=%v, go omitempty=%v)", typ.Name(), fieldName, required[fieldName], optional)
		}
		rawProperty, ok := properties[fieldName].(map[string]any)
		if !ok {
			t.Fatalf("SDK %s field %s has no schema property", typ.Name(), fieldName)
		}
		property := dereferenceJSONSchema(rawProperty, defs)
		if got, want := property["type"], sdkJSONType(typ.Field(i).Type); got != want {
			t.Fatalf("SDK %s JSON type drift for %s: schema=%v SDK=%s", typ.Name(), fieldName, got, want)
		}
		if typ.Field(i).Type == reflect.TypeOf(time.Time{}) && property["format"] != "date-time" {
			t.Fatalf("SDK %s time field %s is not date-time in schema", typ.Name(), fieldName)
		}
	}
	if len(seen) != len(properties) {
		t.Fatalf("SDK %s field count %d != schema properties %d", typ.Name(), len(seen), len(properties))
	}
	for propName := range properties {
		if !seen[propName] {
			t.Fatalf("SDK %s missing JSON field %s", typ.Name(), propName)
		}
	}
}

func TestWorkCaseSchemasCompile(t *testing.T) {
	compile(t, "workcase.schema.json", schemas.WorkCaseSchemaJSON)
	compile(t, "work-plan.schema.json", schemas.WorkPlanSchemaJSON)
	compile(t, "action.schema.json", schemas.ActionSpecSchemaJSON)
}

func TestWorkCaseSDKSchemaParity(t *testing.T) {
	assertSDKShape(t, schemas.WorkCaseSchemaJSON, "WorkCase", reflect.TypeOf(workcase.WorkCase{}))
	assertSDKShape(t, schemas.WorkPlanSchemaJSON, "WorkPlan", reflect.TypeOf(workcase.WorkPlan{}))
	assertSDKShape(t, schemas.WorkPlanSchemaJSON, "Step", reflect.TypeOf(workcase.Step{}))
	assertSDKShape(t, schemas.WorkPlanSchemaJSON, "EvidenceRef", reflect.TypeOf(workcase.EvidenceRef{}))
	assertSDKShape(t, schemas.WorkPlanSchemaJSON, "ReceiptRef", reflect.TypeOf(workcase.ReceiptRef{}))
	assertSDKShape(t, schemas.ActionSpecSchemaJSON, "ActionSpec", reflect.TypeOf(workcase.ActionSpec{}))
	assertSDKShape(t, schemas.ActionSpecSchemaJSON, "Precondition", reflect.TypeOf(workcase.Precondition{}))
}

func TestActionSpecSchema(t *testing.T) {
	sch := compile(t, "action.schema.json", schemas.ActionSpecSchemaJSON)

	mustValidate(t, sch, `{
		"kind": "write",
		"business_capability": "mes.work_order.create",
		"parameters_hash": "sha256:fixture",
		"idempotency_key": "case-1:step-1:v1",
		"expires_at": "2026-08-01T00:00:00Z",
		"failure_policy": "compensate_then_human"
	}`)

	mustValidate(t, sch, `{
		"kind": "read",
		"business_capability": "mes.anomaly.read",
		"parameters_hash": "sha256:fixture",
		"risk": "low",
		"idempotency_key": "case-1:step-1:v1",
		"preconditions": [{"kind": "resource_version", "value_hash": "sha256:pre"}],
		"expires_at": "0001-01-01T00:00:00Z",
		"expected_receipt_schema": "receipt.mes.v1",
		"compensation_action_id": "act-undo-1"
	}`)

	mustReject(t, sch, `{
		"kind": "write",
		"business_capability": "mes.work_order.create",
		"parameters_hash": "sha256:fixture",
		"idempotency_key": "case-1:step-1:v1",
		"expires_at": "2026-08-01T00:00:00Z"
	}`, "write action without failure_policy")

	mustReject(t, sch, `{
		"kind": "read",
		"business_capability": "mes.anomaly.read",
		"parameters_hash": "sha256:fixture",
		"idempotency_key": "case-1:step-1:v1",
		"expires_at": "2026-08-01T00:00:00Z",
		"unknown_field": true
	}`, "unknown field must be rejected by additionalProperties:false")

	mustReject(t, sch, `{
		"kind": "read",
		"business_capability": "MES.Anomaly.Read",
		"parameters_hash": "sha256:fixture",
		"idempotency_key": "case-1:step-1:v1",
		"expires_at": "2026-08-01T00:00:00Z"
	}`, "business_capability must match the lowercase dotted-identifier pattern")

	mustReject(t, sch, `{
		"kind": "read",
		"business_capability": "mes.anomaly.read",
		"parameters_hash": "sha256:fixture",
		"expires_at": "2026-08-01T00:00:00Z"
	}`, "missing idempotency_key")

	mustReject(t, sch, `{
		"kind": "read",
		"business_capability": "mes.anomaly.read",
		"parameters_hash": "sha256:fixture",
		"idempotency_key": "case-1:step-1:v1"
	}`, "missing expires_at (always present on the wire; zero timestamp means not yet bound)")
}

func TestWorkPlanSchema(t *testing.T) {
	sch := compile(t, "work-plan.schema.json", schemas.WorkPlanSchemaJSON)

	mustValidate(t, sch, `{
		"revision": 1,
		"steps": [
			{
				"id": "step-1",
				"status": "pending",
				"evidence": [{"handle": "evd-1", "content_hash": "sha256:abc", "authority": "agentnexus"}],
				"action": {"kind": "read"}
			},
			{"id": "step-2"}
		]
	}`)

	mustReject(t, sch, `{
		"revision": "1",
		"steps": [{"id": "step-1"}]
	}`, "revision must be a JSON integer, not a string")

	mustReject(t, sch, `{
		"revision": 1,
		"steps": []
	}`, "a plan must contain at least one step")

	mustReject(t, sch, `{
		"revision": 1,
		"steps": [{"id": "step-1", "status": "paused"}]
	}`, "unknown step status enum value")
}

func TestWorkCaseSchema(t *testing.T) {
	sch := compile(t, "workcase.schema.json", schemas.WorkCaseSchemaJSON)

	mustValidate(t, sch, `{
		"id": "case-1",
		"enterprise_id": "ent-1",
		"org_scope": "org-unit-7",
		"actor_ref": "user:ops-lead",
		"status": "draft",
		"revision": 1,
		"plans": [{"revision": 1, "steps": [{"id": "step-1"}]}]
	}`)

	mustReject(t, sch, `{
		"id": "case-1",
		"enterprise_id": "ent-1",
		"org_scope": "org-unit-7",
		"actor_ref": "user:ops-lead",
		"status": "archived",
		"revision": 1
	}`, "unknown work case status enum value")
}

// TestWorkPlanValidationEnumsMatchSchema locks the Status and StepStatus Go
// consts to the schema enums so a renamed or added const cannot silently
// drift from the wire contract.
//
// NOTE to future authors: Go reflection cannot enumerate consts, so the
// sdkValues lists below are maintained BY HAND. When you add, remove or
// rename a Status or StepStatus const in sdk/go/workcase/model.go you must
// update the matching list here AND the enum in the schema file, or this
// test fails.
func TestWorkPlanValidationEnumsMatchSchema(t *testing.T) {
	var workCaseDoc map[string]any
	if err := json.Unmarshal(schemas.WorkCaseSchemaJSON, &workCaseDoc); err != nil {
		t.Fatal(err)
	}
	assertEnumParity(t, workCaseDoc, "Status", []string{
		string(workcase.StatusDraft), string(workcase.StatusReviewing), string(workcase.StatusExecuting),
		string(workcase.StatusCompleted), string(workcase.StatusTerminated),
	})

	var workPlanDoc map[string]any
	if err := json.Unmarshal(schemas.WorkPlanSchemaJSON, &workPlanDoc); err != nil {
		t.Fatal(err)
	}
	assertEnumParity(t, workPlanDoc, "StepStatus", []string{
		string(workcase.StepPending), string(workcase.StepRunning), string(workcase.StepCompleted), string(workcase.StepFailed),
	})
}

func assertEnumParity(t *testing.T, doc map[string]any, defName string, sdkValues []string) {
	t.Helper()
	defs := doc["$defs"].(map[string]any)
	def, ok := defs[defName].(map[string]any)
	if !ok {
		t.Fatalf("schema has no $defs/%s", defName)
	}
	rawEnum, ok := def["enum"].([]any)
	if !ok {
		t.Fatalf("$defs/%s has no enum", defName)
	}
	schemaValues := make(map[string]bool, len(rawEnum))
	for _, v := range rawEnum {
		schemaValues[v.(string)] = true
	}
	sdkSeen := make(map[string]bool, len(sdkValues))
	for _, v := range sdkValues {
		sdkSeen[v] = true
		if !schemaValues[v] {
			t.Fatalf("SDK %s const %q missing from schema enum", defName, v)
		}
	}
	for v := range schemaValues {
		if !sdkSeen[v] {
			t.Fatalf("schema %s enum %q has no matching SDK const", defName, v)
		}
	}
}
