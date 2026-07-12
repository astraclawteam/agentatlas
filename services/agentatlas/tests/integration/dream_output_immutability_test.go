package integration

import (
	"context"
	"os"
	"testing"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDreamOutputImmutabilityPostgres(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	ent, space, wf, policy, run, summary, pointer, annotation := newID("ent-immutable"), newID("space-immutable"), newID("wf-immutable"), newID("policy-immutable"), newID("run-immutable"), newID("summary-immutable"), newID("ev-immutable"), newID("annotation-immutable")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: ent, Name: "Immutable"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{ID: space, EnterpriseID: ent, Kind: "department", Name: "Immutable", OrgScope: "department:immutable", OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{ID: wf, EnterpriseID: ent, Name: "Immutable", Kind: "dream", CreatedBy: "test", Draft: []byte(`{"nodes":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{WorkflowID: wf, Version: 1, Definition: []byte(`{"nodes":[]}`), RiskLevel: "low", PublishedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{ID: policy, EnterpriseID: ent, OrgScope: "department:immutable", Status: "published", Draft: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{PolicyID: policy, Version: 1, Definition: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{ID: run, PolicyID: policy, Version: 1, EnterpriseID: ent, Status: "succeeded", WindowStart: pgtype.Timestamptz{Time: start, Valid: true}, WindowEnd: pgtype.Timestamptz{Time: start.Add(time.Hour), Valid: true}, OrgUnitID: "department:immutable", PolicyVersion: 1, WorkflowID: wf, WorkflowVersion: 1, Timezone: "UTC", InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"managers","org_unit_ids":["department:immutable"],"masked_field_count":0}`), ModelRoute: "workflow/" + wf, ModelVersion: "v1", Attempt: 1, Coverage: []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`), MissingInputs: []byte(`[]`), IdempotencyKey: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{ID: pointer, EnterpriseID: ent, ResourceType: "dream_sealed_summary", ResourceRef: "object://immutable", SourceSystem: "object-storage", RequiredScopes: []string{"dream:evidence:read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{ID: summary, RunID: run, EnterpriseID: ent, SpaceID: space, Layer: "display", SummaryText: "original", RiskSignals: []byte(`[]`), Facts: []byte(`[]`), Themes: []byte(`[]`), Trends: []byte(`[]`), Todos: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{DreamSummaryID: summary, EvidencePointerID: pointer}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDreamAnnotation(ctx, db.CreateDreamAnnotationParams{ID: annotation, EnterpriseID: ent, RunID: run, AnnotationType: "comment", Body: "original", CreatedBy: "manager"}); err != nil {
		t.Fatal(err)
	}
	mutations := []struct{ name, statement, id string }{
		{"summary update", `update dream_summaries set summary_text='changed' where id=$1`, summary},
		{"summary delete", `delete from dream_summaries where id=$1`, summary},
		{"link update", `update dream_evidence_pointers set evidence_pointer_id=$2 where dream_summary_id=$1`, summary},
		{"link delete", `delete from dream_evidence_pointers where dream_summary_id=$1`, summary},
		{"annotation update", `update dream_run_annotations set body='changed' where id=$1`, annotation},
		{"annotation delete", `delete from dream_run_annotations where id=$1`, annotation},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, mutation.statement, mutation.id, pointer); err == nil {
				t.Fatal("immutable record mutation accepted")
			}
		})
	}
	if _, err := q.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{ID: newID("replacement"), RunID: run, EnterpriseID: ent, SpaceID: space, Layer: "display", SummaryText: "replacement", RiskSignals: []byte(`[]`)}); err == nil {
		t.Fatal("replacement summary layer accepted")
	}
}
