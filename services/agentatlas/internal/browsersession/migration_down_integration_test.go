package browsersession

import (
	"context"
	"database/sql"
	"os"
	"testing"

	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestMigrationTenDownTransformsLowRiskUpwardReview(t *testing.T) {
	dsn := os.Getenv("ATLAS_TASK8_MIGRATION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ATLAS_TASK8_MIGRATION_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO enterprises(id,name) VALUES('ent-migration-down','Migration Down'); INSERT INTO change_drafts(id,enterprise_id,org_unit_id,resource_type,resource_id,action,requester_user_id,origin,permission_mode,state,proposed_content) VALUES('chg-low-up','ent-migration-down','team','knowledge_entry','kb-1','update','requester','direct_edit','direct_edit','submitted','{}'); INSERT INTO change_reviews(id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,review_mode,state,org_path) VALUES('review-low-up','ent-migration-down','chg-low-up',1,'manager','low','upward_review','pending','["team","department"]')`); err != nil {
		t.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.DownToContext(ctx, db, "migrations", 9); err != nil {
		t.Fatalf("down migration rejected valid Task 8 data: %v", err)
	}
	var mode, risk string
	var reviewer sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT review_mode,risk_level,reviewer_user_id FROM change_reviews WHERE id='review-low-up'`).Scan(&mode, &risk, &reviewer); err != nil {
		t.Fatal(err)
	}
	if mode != "single_confirmation" || risk != "low" || reviewer.Valid {
		t.Fatalf("transformed review mode=%q risk=%q reviewer=%v", mode, risk, reviewer)
	}
}
