package nexusclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// publishedGatewayRuntimeDigest is the LF-normalized sha256 of AgentNexus's
// frozen api/openapi/gateway-runtime.yaml exactly as recorded in that
// repository's api/contract.lock. Pinning it here is what makes the parity
// assertion below mean anything: without it a stale or hand-edited snapshot
// would silently "pass".
const publishedGatewayRuntimeDigest = "sha256:fa3932f3757989c8ff2a8f98db162025d44ded1398eef03129047cab0c1292c0"

const publishedGatewayRuntimeSnapshot = "agentnexus-gateway-runtime-published.yaml"

func TestPublishedGatewayRuntimeSnapshotMatchesAgentNexusContractLock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", publishedGatewayRuntimeSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	// AgentNexus's contract lock declares normalization: lf.
	sum := sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != publishedGatewayRuntimeDigest {
		t.Fatalf("published snapshot digest drift\n got=%s\nwant=%s", got, publishedGatewayRuntimeDigest)
	}
}

// TestAgentAtlasConsumerContractPathsExistInPublishedGatewayRuntime is the
// consumer half of ELC-NEXUS-1: every runtime path AgentAtlas declares it will
// call must actually exist in the frozen AgentNexus contract. AgentAtlas was
// written against the earlier agentnexus-client.yaml PROPOSAL contract, so this
// fails until the consumer contract is migrated onto the published surface.
func TestAgentAtlasConsumerContractPathsExistInPublishedGatewayRuntime(t *testing.T) {
	consumer := readOpenAPIMap(t, filepath.Join("..", "..", "api", "openapi", "agentnexus-client.yaml"))
	published := readOpenAPIMap(t, filepath.Join("testdata", publishedGatewayRuntimeSnapshot))

	// notYetMigrated mirrors the shrinking allowlist of the behavioural test
	// next to this one. Each entry is a surface whose DECLARATION still names a
	// retired operation; removing an entry is the definition of "this surface's
	// declaration is migrated". The list is asserted to be exact in both
	// directions, so neither a silent regression nor a stale entry can hide.
	notYetMigrated := map[string]bool{
		"/v1/approvals/resolve":   true, // -> /v1/approvals/transmissions (Task 0E retired resolution)
		"/v1/actions/request":     true, // -> /v1/runtime/act
		"/v1/actions/receipt":     true, // -> /v1/runtime/receipts/{receipt_ref}
		"/v1/actions/observation": true, // -> folded into the receipt surface
	}
	publishedPaths := openAPIPathSet(t, published)
	var missing []string
	for _, path := range sortedKeys(openAPIPathSet(t, consumer)) {
		if !publishedPaths[path] {
			missing = append(missing, path)
		}
	}
	for _, path := range missing {
		if !notYetMigrated[path] {
			t.Fatalf("consumer declares %q, absent from the published AgentNexus contract\npublished paths: %v",
				path, sortedKeys(publishedPaths))
		}
		delete(notYetMigrated, path)
	}
	for path := range notYetMigrated {
		t.Fatalf("%s is listed as not-yet-migrated but is no longer declared off-contract; remove it from the allowlist", path)
	}
}

func openAPIPathSet(t *testing.T, document map[string]any) map[string]bool {
	t.Helper()
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("openapi document has no paths object")
	}
	set := make(map[string]bool, len(paths))
	for path := range paths {
		set[path] = true
	}
	return set
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
