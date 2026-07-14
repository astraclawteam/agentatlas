// Package outcome_test proves the embedded outcome/lineage JSON Schemas
// compile, behave (valid documents pass, boundary-violating documents fail)
// and mirror the SDK types field-for-field — the same accept/reject and
// reflection-parity discipline services/agentatlas/schemas/workcase/parity_test.go
// applies to the WorkCase family. The parity half re-implements the
// unexported assertSDKShape/assertEnumParity helpers locally because this
// test lives one directory below the schemas package that embeds the JSON.
package outcome_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/schemas"
)

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

// compileDef compiles a single $def of a schema as the validation root, so a
// multi-type file (lineage.schema.json) can be behaviorally exercised one
// type at a time.
func compileDef(t *testing.T, resource string, schemaJSON []byte, def string) *jsonschema.Schema {
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
	sch, err := compiler.Compile(resource + "#/$defs/" + def)
	if err != nil {
		t.Fatalf("%s#/$defs/%s: compile: %v", resource, def, err)
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

// --- compile & parity ------------------------------------------------------

func TestOutcomeSchemasCompile(t *testing.T) {
	compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	compile(t, "lineage.schema.json", schemas.LineageSchemaJSON)
}

func TestOutcomeSDKSchemaParity(t *testing.T) {
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "Outcome", reflect.TypeOf(outcome.Outcome{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "OutcomeClaim", reflect.TypeOf(outcome.OutcomeClaim{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "GoalRef", reflect.TypeOf(outcome.GoalRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "EvidenceRef", reflect.TypeOf(outcome.EvidenceRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "ObservationRef", reflect.TypeOf(outcome.ObservationRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "ReceiptRef", reflect.TypeOf(outcome.ReceiptRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "BlockerRef", reflect.TypeOf(outcome.BlockerRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "ContributionRef", reflect.TypeOf(outcome.ContributionRef{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "ConfidenceComponent", reflect.TypeOf(outcome.ConfidenceComponent{}))
	assertSDKShape(t, schemas.OutcomeSchemaJSON, "OutcomeRevisionRef", reflect.TypeOf(outcome.OutcomeRevisionRef{}))

	assertSDKShape(t, schemas.LineageSchemaJSON, "LineageEndpoint", reflect.TypeOf(outcome.LineageEndpoint{}))
	assertSDKShape(t, schemas.LineageSchemaJSON, "LineageNode", reflect.TypeOf(outcome.LineageNode{}))
	assertSDKShape(t, schemas.LineageSchemaJSON, "LineageEdge", reflect.TypeOf(outcome.LineageEdge{}))
	assertSDKShape(t, schemas.LineageSchemaJSON, "ProjectionEvent", reflect.TypeOf(outcome.ProjectionEvent{}))
	assertSDKShape(t, schemas.LineageSchemaJSON, "ProjectionWatermark", reflect.TypeOf(outcome.ProjectionWatermark{}))
}

// TestOutcomeEnumsMatchSchema locks the SDK enum consts to the schema enums.
//
// LIMITATION (documented in evidence notes.md): Go reflection cannot enumerate
// consts, so each SDK enum's membership comes from a hand-maintained slice
// helper (OutcomeStatuses / NodeTypes / EdgeTypes / ProjectionEventKinds). This
// test proves that slice equals the schema enum, but it CANNOT prove the slice
// itself is complete: a new status const that is NOT added to OutcomeStatuses()
// escapes both the slice and the schema and would not be caught here. Adding or
// renaming a const therefore requires updating its ...() helper AND the schema
// together.
func TestOutcomeEnumsMatchSchema(t *testing.T) {
	var outcomeDoc map[string]any
	if err := json.Unmarshal(schemas.OutcomeSchemaJSON, &outcomeDoc); err != nil {
		t.Fatal(err)
	}
	statuses := make([]string, 0, len(outcome.OutcomeStatuses()))
	for _, s := range outcome.OutcomeStatuses() {
		statuses = append(statuses, string(s))
	}
	assertEnumParity(t, outcomeDoc, "OutcomeStatus", statuses)

	var lineageDoc map[string]any
	if err := json.Unmarshal(schemas.LineageSchemaJSON, &lineageDoc); err != nil {
		t.Fatal(err)
	}
	nodeTypes := make([]string, 0, len(outcome.NodeTypes()))
	for _, n := range outcome.NodeTypes() {
		nodeTypes = append(nodeTypes, string(n))
	}
	assertEnumParity(t, lineageDoc, "NodeType", nodeTypes)
	edgeTypes := make([]string, 0, len(outcome.EdgeTypes()))
	for _, e := range outcome.EdgeTypes() {
		edgeTypes = append(edgeTypes, string(e))
	}
	assertEnumParity(t, lineageDoc, "EdgeType", edgeTypes)
	kinds := make([]string, 0, len(outcome.ProjectionEventKinds()))
	for _, k := range outcome.ProjectionEventKinds() {
		kinds = append(kinds, string(k))
	}
	assertEnumParity(t, lineageDoc, "ProjectionEventKind", kinds)
}

// --- behavioral: outcome.schema.json (root = Outcome) ----------------------

func validOutcomeDoc() string {
	return `{
		"id": "oc_abc",
		"tenant": "ent-1",
		"outcome_key": "case-42.goal.close_anomaly",
		"revision": 1,
		"claim": {
			"goal": {"tenant": "ent-1", "goal_key": "mes.close_anomaly", "goal_version": 3},
			"status": "satisfied",
			"rule_version": "outcome-policy-rev-7",
			"observations": [{"handle": "obs-1", "observation_hash": "sha256:1", "authority": "system_of_record", "signature_key_id": "nexus-key-1", "observed_at": "2026-07-13T11:00:00Z"}]
		},
		"work_case_id": "case-42",
		"work_case_revision": 5,
		"work_plan_revision": 2,
		"operating_map_version": 4,
		"org_version": 1,
		"decided_at": "2026-07-13T12:00:00Z"
	}`
}

func TestOutcomeSchemaAcceptsValidDocument(t *testing.T) {
	sch := compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	mustValidate(t, sch, validOutcomeDoc())
}

func TestOutcomeSchemaRejectsUnknownStatus(t *testing.T) {
	sch := compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	mustReject(t, sch, strings.Replace(validOutcomeDoc(), `"status": "satisfied"`, `"status": "succeeded"`, 1),
		"succeeded is an ActionStatus, not an OutcomeStatus")
}

func TestOutcomeSchemaRejectsSatisfiedWithoutObservation(t *testing.T) {
	sch := compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	mustReject(t, sch, `{
		"id": "oc_x", "tenant": "ent-1", "outcome_key": "k", "revision": 1,
		"claim": {"goal": {"tenant": "ent-1", "goal_key": "g", "goal_version": 1}, "status": "satisfied", "rule_version": "r"},
		"work_case_id": "case-42", "work_case_revision": 1, "work_plan_revision": 1,
		"operating_map_version": 1, "org_version": 0, "decided_at": "2026-07-13T12:00:00Z"
	}`, "satisfied outcome must carry at least one observation")
}

func TestOutcomeSchemaRejectsMissingGoalVersion(t *testing.T) {
	sch := compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	mustReject(t, sch, strings.Replace(validOutcomeDoc(), `"goal_version": 3`, `"goal_version": 0`, 1),
		"goal_version must be >= 1")
}

func TestOutcomeSchemaRejectsUnknownField(t *testing.T) {
	sch := compile(t, "outcome.schema.json", schemas.OutcomeSchemaJSON)
	mustReject(t, sch, strings.Replace(validOutcomeDoc(), `"org_version": 1,`, `"org_version": 1, "mystery": true,`, 1),
		"additionalProperties:false must reject unknown fields")
}

// --- behavioral: lineage.schema.json ($defs compiled individually) ---------

func TestLineageEdgeSchemaRejectsUnversionedEndpoint(t *testing.T) {
	sch := compileDef(t, "lineage.schema.json", schemas.LineageSchemaJSON, "LineageEdge")
	mustValidate(t, sch, `{
		"id": "oe_1", "tenant": "ent-1", "edge_type": "concerns",
		"from": {"tenant": "ent-1", "node_type": "outcome", "business_id": "o", "revision": 1},
		"to": {"tenant": "ent-1", "node_type": "goal", "business_id": "g", "revision": 2}
	}`)
	mustReject(t, sch, `{
		"id": "oe_1", "tenant": "ent-1", "edge_type": "concerns",
		"from": {"tenant": "ent-1", "node_type": "outcome", "business_id": "o", "revision": 1},
		"to": {"tenant": "ent-1", "node_type": "goal", "business_id": "g", "revision": 0}
	}`, "endpoint revision must be >= 1 (versioned)")
}

func TestLineageEdgeSchemaRejectsUnknownEdgeType(t *testing.T) {
	sch := compileDef(t, "lineage.schema.json", schemas.LineageSchemaJSON, "LineageEdge")
	mustReject(t, sch, `{
		"id": "oe_1", "tenant": "ent-1", "edge_type": "mystery",
		"from": {"tenant": "ent-1", "node_type": "outcome", "business_id": "o", "revision": 1},
		"to": {"tenant": "ent-1", "node_type": "goal", "business_id": "g", "revision": 1}
	}`, "edge_type is a closed enum")
}

func TestLineageNodeSchemaRejectsUnknownNodeType(t *testing.T) {
	sch := compileDef(t, "lineage.schema.json", schemas.LineageSchemaJSON, "LineageNode")
	mustReject(t, sch, `{"id": "on_1", "tenant": "ent-1", "node_type": "cypher_internal", "business_id": "x", "revision": 1}`,
		"node_type is a closed enum")
}

func TestProjectionEventSchemaRejectsUnknownKind(t *testing.T) {
	sch := compileDef(t, "lineage.schema.json", schemas.LineageSchemaJSON, "ProjectionEvent")
	mustValidate(t, sch, `{
		"tenant": "ent-1", "sequence": 1, "kind": "tombstone",
		"subject_type": "outcome", "subject_id": "on_1", "subject_revision": 1,
		"payload_hash": "sha256:1", "recorded_at": "2026-07-13T12:00:00Z"
	}`)
	mustReject(t, sch, `{
		"tenant": "ent-1", "sequence": 1, "kind": "edit_in_place",
		"subject_type": "outcome", "subject_id": "on_1", "subject_revision": 1,
		"payload_hash": "sha256:1", "recorded_at": "2026-07-13T12:00:00Z"
	}`, "projection event kind is a closed enum with no in-place edit")
}
