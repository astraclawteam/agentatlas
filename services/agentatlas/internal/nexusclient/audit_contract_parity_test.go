package nexusclient

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"gopkg.in/yaml.v3"
)

func TestAgentAtlasAuditClientMatchesPublishedAgentNexusSnapshot(t *testing.T) {
	consumer := readOpenAPIMap(t, filepath.Join("..", "..", "api", "openapi", "agentnexus-client.yaml"))
	published := readOpenAPIMap(t, filepath.Join("testdata", "agentnexus-audit-evidence-published.yaml"))
	for _, path := range [][]string{
		{"paths", "/v1/audit/evidence"},
		{"components", "securitySchemes", "agentAtlasServiceSecret"},
		{"components", "schemas", "AuditEvidenceAction"},
		{"components", "schemas", "AuditEvidenceRequest"},
		{"components", "schemas", "AuditEvidenceResponse"},
	} {
		got := nestedOpenAPIValue(t, consumer, path...)
		want := nestedOpenAPIValue(t, published, path...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("AgentNexus contract drift at %v\nconsumer=%#v\npublished=%#v", path, got, want)
		}
	}
	publishedActions := nestedOpenAPIValue(t, published, "components", "schemas", "AuditEvidenceAction").(map[string]any)["enum"]
	sdkActions := []any{
		string(nexus.AuditWorkflowDraftCreated), string(nexus.AuditWorkflowVersionPublished), string(nexus.AuditDreamPolicyCreated),
		string(nexus.AuditDreamPolicyCreateRequested), string(nexus.AuditDreamJobRun), string(nexus.AuditRetrievalPlanCreated),
		string(nexus.AuditEvidenceLocated), string(nexus.AuditEvidenceRead), string(nexus.AuditAnswerTraceCreated),
		string(nexus.AuditSensitiveArtifactParsed), string(nexus.AuditVisibilityRuleChanged),
		string(nexus.AuditGovernanceChangeDecided),
	}
	if !reflect.DeepEqual(sdkActions, publishedActions) {
		t.Fatalf("SDK audit actions=%v published=%v", sdkActions, publishedActions)
	}
}

func readOpenAPIMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func nestedOpenAPIValue(t *testing.T, root map[string]any, path ...string) any {
	t.Helper()
	var value any = root
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%v is not an object", path)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("%v missing %q", path, key)
		}
	}
	return value
}
