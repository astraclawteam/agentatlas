// Package schemas_test proves the public JSON Schemas compile and behave:
// valid documents pass, boundary-violating documents fail. It also checks the
// OpenAPI contracts are well-formed YAML with the expected surfaces.
package schemas_test

import (
	"os"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func compile(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	sch, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
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

func TestWorkflowSchema(t *testing.T) {
	sch := compile(t, "workflow/workflow.schema.json")

	mustValidate(t, sch, `{
		"workflow_id": "wf_mes_sop",
		"version": 1,
		"kind": "sop",
		"nodes": [
			{"id": "in", "type": "input.evidence_pointer"},
			{"id": "parse", "type": "parser.document", "config": {"provider_hint": "mineru"}},
			{"id": "extract", "type": "transform.extract_sop"},
			{"id": "confirm", "type": "human.confirm", "requires_confirmation": true},
			{"id": "trace", "type": "trace.append"}
		],
		"edges": [
			{"from": "in", "to": "parse"},
			{"from": "parse", "to": "extract"},
			{"from": "extract", "to": "confirm"},
			{"from": "confirm", "to": "trace", "condition": "approved"}
		],
		"risk_level": "medium"
	}`)

	mustReject(t, sch, `{
		"workflow_id": "wf_bad",
		"version": 1,
		"kind": "sop",
		"nodes": [{"id": "x", "type": "custom.hack"}],
		"edges": [],
		"risk_level": "low"
	}`, "unknown node type")

	mustReject(t, sch, `{
		"workflow_id": "wf_bad2",
		"version": 1,
		"kind": "sop",
		"nodes": [{"id": "x", "type": "input.manual"}],
		"edges": []
	}`, "missing risk_level")

	mustReject(t, sch, `{
		"workflow_id": "wf_bad3",
		"version": 1,
		"kind": "etl",
		"nodes": [{"id": "x", "type": "input.manual"}],
		"edges": [],
		"risk_level": "low"
	}`, "unknown kind")
}

func TestParserProviderSchema(t *testing.T) {
	sch := compile(t, "parser/parser-provider.schema.json")

	mustValidate(t, sch, `{
		"provider_id": "docling",
		"capabilities": ["pdf.layout", "pdf.table", "image.ocr", "office.parse"],
		"input_types": ["application/pdf", "image/png"],
		"output_schema": "atlas_document.v1",
		"max_file_size_mb": 200,
		"supports_private": true
	}`)

	mustReject(t, sch, `{
		"provider_id": "docling",
		"capabilities": ["pdf.layout"],
		"input_types": ["application/pdf"],
		"output_schema": "something.v2",
		"max_file_size_mb": 200,
		"supports_private": true
	}`, "wrong output schema const")

	mustReject(t, sch, `{
		"provider_id": "Docling!",
		"capabilities": ["pdf.layout"],
		"input_types": ["application/pdf"],
		"output_schema": "atlas_document.v1",
		"max_file_size_mb": 200,
		"supports_private": true
	}`, "invalid provider_id pattern")
}

func TestAtlasDocumentSchema(t *testing.T) {
	sch := compile(t, "atlasdocument/atlas-document.schema.json")

	mustValidate(t, sch, `{
		"atlas_document_id": "doc_1",
		"artifact_id": "file_1",
		"source_hash": "sha256:abc",
		"content_type": "application/pdf",
		"provider": "docling",
		"confidence": 0.92,
		"blocks": [
			{"block_id": "b1", "type": "heading", "order": 0, "page": 1, "text": "MES 异常工单排查"}
		],
		"tables": [
			{"table_id": "t1", "page": 2, "n_rows": 3, "n_cols": 2,
			 "cells": [{"row": 0, "col": 0, "text": "风险点"}]}
		],
		"summaries": [
			{"summary_id": "s1", "level": "display", "text": "MES 异常工单排查 SOP"}
		],
		"evidence_pointers": [
			{"enterprise_id": "ent_1", "resource_type": "document",
			 "resource_ref": "fs://sop/mes.pdf", "source_system": "filesystem"}
		]
	}`)

	mustReject(t, sch, `{
		"atlas_document_id": "doc_2",
		"artifact_id": "file_2",
		"source_hash": "sha256:def",
		"content_type": "application/pdf",
		"blocks": [{"block_id": "b1", "type": "text"}]
	}`, "block missing order")

	mustReject(t, sch, `{
		"artifact_id": "file_3",
		"source_hash": "sha256:x",
		"content_type": "application/pdf"
	}`, "missing atlas_document_id")
}

func TestOpenAPIContractsWellFormed(t *testing.T) {
	cases := map[string][]string{
		"../api/openapi/atlas-runtime.yaml": {
			"/v1/answer", "/v1/retrieval/plans", "/v1/work-briefs", "/v1/artifacts/jobs",
			"/v1/spaces/{id}", "/v1/spaces/{id}/timeline", "/v1/traces/{id}",
		},
		"../api/openapi/atlas-agent.yaml": {
			"/v1/agent/runs", "/v1/agent/runs/{id}/messages", "/v1/agent/runs/{id}/confirmations",
			"/v1/workflows", "/v1/workflows/{id}/publish", "/v1/workflows/{id}/runs", "/v1/dream-policies",
		},
		"../api/openapi/agentnexus-client.yaml": {
			"/v1/tickets/verify", "/v1/runtime/locate", "/v1/runtime/read",
			"/v1/audit/evidence", "/v1/org-events",
		},
	}
	for file, wantPaths := range cases {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var doc struct {
			OpenAPI string         `yaml:"openapi"`
			Paths   map[string]any `yaml:"paths"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if !strings.HasPrefix(doc.OpenAPI, "3.") {
			t.Fatalf("%s: openapi version = %q", file, doc.OpenAPI)
		}
		for _, p := range wantPaths {
			if _, ok := doc.Paths[p]; !ok {
				t.Fatalf("%s: missing path %s", file, p)
			}
		}
	}
}
