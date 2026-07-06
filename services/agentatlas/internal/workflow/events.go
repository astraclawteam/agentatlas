package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// writeEvent records one node transition; every transition is persisted so a
// run is auditable end to end.
func writeEvent(ctx context.Context, store Store, runID, nodeID, status string, detail map[string]any) error {
	raw := []byte(`{}`)
	if detail != nil {
		var err error
		raw, err = json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("encode event detail: %w", err)
		}
	}
	if _, err := store.InsertWorkflowRunEvent(ctx, db.InsertWorkflowRunEventParams{
		RunID: runID, NodeID: nodeID, Status: status, Detail: raw,
	}); err != nil {
		return fmt.Errorf("write run event %s/%s: %w", nodeID, status, err)
	}
	return nil
}
