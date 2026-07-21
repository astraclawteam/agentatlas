package nexusclient

import (
	"testing"

	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// TestNexusPublicSDKResolves proves the cross-repo public contract module is
// consumable before any migration work depends on it.
func TestNexusPublicSDKResolves(t *testing.T) {
	req := nexusruntime.ActionRequest{}
	if err := req.Validate(); err == nil {
		t.Fatal("empty ActionRequest must not validate")
	}
	t.Logf("nexus public SDK reachable; empty request rejected as expected")
}
