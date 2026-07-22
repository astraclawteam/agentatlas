package nexusclient

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"gopkg.in/yaml.v3"
)

// TestAgentAtlasAuditClientMatchesPublishedAgentNexusSnapshot holds AgentAtlas's
// audit-append declaration against the SAME digest-pinned AgentNexus snapshot
// every other parity test in this package uses.
//
// It used to compare against testdata/agentnexus-audit-evidence-published.yaml,
// a second "published" fixture with no digest assertion anywhere and exactly one
// reference in the repository — this line. Nothing held it to AgentNexus, so it
// had quietly become a copy of a superseded contract, and the test's job was to
// confirm that AgentAtlas still agreed with it. It did, and every disagreement
// that mattered was invisible: the fixture named a security scheme AgentNexus
// had renamed, required an enterprise_id that AgentNexus's handler rejects
// outright, and carried five more members the frozen schema does not define.
// A green run meant the consumer contract matched a file no one maintained.
//
// The pinned snapshot's digest is asserted in gateway_runtime_parity_test.go, so
// a stale copy fails there before this test can agree with it.
func TestAgentAtlasAuditClientMatchesPublishedAgentNexusSnapshot(t *testing.T) {
	consumer := readOpenAPIMap(t, filepath.Join("..", "..", "api", "openapi", "agentnexus-client.yaml"))
	published := readOpenAPIMap(t, filepath.Join("testdata", publishedGatewayRuntimeSnapshot))
	for _, path := range [][]string{
		{"paths", "/v1/audit/evidence"},
		{"components", "securitySchemes", "trustedServiceSecret"},
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

	// The SDK's AuditAction constants must be exactly the published enum plus
	// the shrinking allowlist below — asserted in both directions, so neither a
	// constant that quietly drops off the wire nor a stale allowlist entry can
	// hide.
	//
	// notYetFrozen is not an exemption. governance_change_decided is a real
	// AgentAtlas action (NexusAuditAppender.AppendDecision records every
	// admin-queue approve/reject with it) that AgentNexus has never accepted:
	// its internal/audit.ValidAction switch lists the same eleven values as the
	// enum and answers anything else 400 invalid_request. Removing this entry is
	// the definition of "AgentNexus froze it"; until then, that one append
	// cannot succeed against a real AgentNexus and this names why.
	notYetFrozen := map[string]bool{
		"governance_change_decided": true,
	}
	publishedActions, ok := nestedOpenAPIValue(t, published, "components", "schemas", "AuditEvidenceAction").(map[string]any)["enum"].([]any)
	if !ok {
		t.Fatal("published AuditEvidenceAction has no enum list")
	}
	frozen := make(map[string]bool, len(publishedActions))
	for _, action := range publishedActions {
		frozen[action.(string)] = true
	}
	sdkActions := []string{
		string(nexus.AuditWorkflowDraftCreated), string(nexus.AuditWorkflowVersionPublished), string(nexus.AuditDreamPolicyCreated),
		string(nexus.AuditDreamPolicyCreateRequested), string(nexus.AuditDreamJobRun), string(nexus.AuditRetrievalPlanCreated),
		string(nexus.AuditEvidenceLocated), string(nexus.AuditEvidenceRead), string(nexus.AuditAnswerTraceCreated),
		string(nexus.AuditSensitiveArtifactParsed), string(nexus.AuditVisibilityRuleChanged),
		string(nexus.AuditGovernanceChangeDecided),
	}
	declared := make(map[string]bool, len(sdkActions))
	for _, action := range sdkActions {
		declared[action] = true
		if frozen[action] {
			continue
		}
		if !notYetFrozen[action] {
			t.Fatalf("SDK declares audit action %q, absent from the frozen AgentNexus enum %v", action, publishedActions)
		}
		delete(notYetFrozen, action)
	}
	for action := range notYetFrozen {
		t.Fatalf("%s is listed as not-yet-frozen but the SDK no longer declares it; remove it from the allowlist", action)
	}
	for action := range frozen {
		if !declared[action] {
			t.Fatalf("frozen AgentNexus enum has %q and the SDK does not declare it", action)
		}
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
