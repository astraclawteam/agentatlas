package integration

import (
	"context"
	"fmt"
	"os"
	"testing"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestBrowserKnowledgeSearchPostgresLiteralMetacharactersAndBounds(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	queries := db.New(pool)
	ent, space, scope := newID("ent_search"), newID("space_search"), "department:"+newID("dept_search")
	if _, err = queries.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: ent, Name: "搜索测试企业"}); err != nil {
		t.Fatal(err)
	}
	if _, err = queries.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{ID: space, EnterpriseID: ent, Kind: "department", Name: "搜索测试部", OrgScope: scope, OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}

	for i, title := range []string{"进度 100% 完成", "编号 A_B", `路径 A\B`, "普通标题"} {
		if _, err = pool.Exec(ctx, `INSERT INTO sops(id,enterprise_id,title,org_scope) VALUES($1,$2,$3,$4)`, fmt.Sprintf("%s-special-%d", space, i), ent, title, scope); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 101; i++ {
		if _, err = pool.Exec(ctx, `INSERT INTO sops(id,enterprise_id,title,org_scope,updated_at) VALUES($1,$2,$3,$4,now()+$5::interval)`, fmt.Sprintf("%s-bound-%03d", space, i), ent, fmt.Sprintf("边界标题 %03d", i), scope, fmt.Sprintf("%d seconds", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO method_outlines(id,enterprise_id,title,outline,org_scope,updated_at) VALUES($1,$2,$3,'{}',$4,now()+interval '1 day')`, space+"-outline", ent, "边界标题 最新说明", scope); err != nil {
		t.Fatal(err)
	}

	search := func(value string, limit int32) []db.ListBrowserKnowledgeItemsRow {
		rows, queryErr := queries.ListBrowserKnowledgeItems(ctx, db.ListBrowserKnowledgeItemsParams{EnterpriseID: ent, SpaceID: space, OrgScope: scope, SearchQuery: value, ResultLimit: limit})
		if queryErr != nil {
			t.Fatalf("search %q: %v", value, queryErr)
		}
		return rows
	}
	for _, tc := range []struct{ query, title string }{{`\%`, "进度 100% 完成"}, {`\_`, "编号 A_B"}, {`\\`, `路径 A\B`}} {
		rows := search(tc.query, 101)
		if len(rows) != 1 || rows[0].SummaryText != tc.title {
			t.Fatalf("literal %q rows=%v", tc.query, rows)
		}
	}
	rows := search("边界标题", 101)
	if len(rows) != 101 {
		t.Fatalf("101 guard query returned %d", len(rows))
	}
	rows = search("边界标题", 100)
	if len(rows) != 100 || rows[0].SummaryText != "边界标题 最新说明" {
		t.Fatalf("bounded ordering rows=%d first=%q", len(rows), rows[0].SummaryText)
	}
}
