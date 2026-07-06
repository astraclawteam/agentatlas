package artifacts

import (
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// BuildEvidencePointer references a resource without copying its content.
// resource_ref points at the AgentNexus-locatable location (object key for
// uploads, source-system URI for connector-read artifacts).
func BuildEvidencePointer(enterpriseID, resourceType, resourceRef, sourceSystem, contentHash string, scopes []string) db.CreateEvidencePointerParams {
	if scopes == nil {
		scopes = []string{}
	}
	return db.CreateEvidencePointerParams{
		ID:             newID("ev"),
		EnterpriseID:   enterpriseID,
		ResourceType:   resourceType,
		ResourceRef:    resourceRef,
		SourceSystem:   sourceSystem,
		ContentHash:    contentHash,
		RequiredScopes: scopes,
	}
}
