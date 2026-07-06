// Package spaces maintains knowledge spaces driven by the AgentNexus org
// graph: employee, project group, department, business unit, and company
// spaces. The org graph truth source stays in AgentNexus; AgentAtlas caches
// only org_scope/org_version needed for sync and audit.
package spaces

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// ScopeString is the canonical org_scope encoding: "<kind>:<scope_id>".
func ScopeString(kind nexus.OrgScopeKind, scopeID string) string {
	return fmt.Sprintf("%s:%s", kind, scopeID)
}

func newSpaceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return "spc_" + hex.EncodeToString(b)
}
