package operatingmap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	model "github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
)

func newID(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-"))
}

// TestOperatingMapMigrationUpDownUp proves migration 000015 is reversible:
// up creates operating_map_entries, down removes it (on an empty table),
// and up again recreates it — before any functional test below writes rows.
func TestOperatingMapMigrationUpDownUp(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)

	if err := goose.DownToContext(ctx, db, "migrations", 14); err != nil {
		t.Fatalf("migrate down to 14: %v", err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='operating_map_entries')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("expected operating_map_entries to be dropped by the down migration")
	}

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='operating_map_entries')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("expected operating_map_entries to exist after re-applying the up migration")
	}
}

// This file reuses the mesEntry and lineStoppageEntry fixtures declared in
// service_test.go (same package, no build tags), so the Postgres legs and
// the in-memory legs exercise byte-identical inputs.

// TestPostgresOperatingMapPublishAndResolveContracts exercises the real
// PostgresStore end to end: the mandated Chinese-intent resolution and
// conflicting-authority-rule rejection, plus the mandated expired-rule,
// org-version-change and tenant-isolation cases, the supersession
// determinism and touching-interval boundary semantics of the conflict
// gate, tenant/scope isolation of the conflict gate itself, the
// append-only immutability triggers (UPDATE, DELETE and TRUNCATE), and
// the migration's down-with-data guard.
func TestPostgresOperatingMapPublishAndResolveContracts(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store, err := NewPostgresStore(pool, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	svc, err := NewService(store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	entA := newID("ent-opm-a")
	entB := newID("ent-opm-b")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Operating Map A'),($2,'Operating Map B')`, entA, entB); err != nil {
		t.Fatalf("seed enterprises: %v", err)
	}
	scope := "department:mes-floor-1"

	// --- Mandated: "生产异常状态" resolves to a semantic MES data need and
	// business keys, with no connector/API/URL/credential-shaped strings.
	published, err := svc.Publish(ctx, mesEntry(entA, scope, 1, now.Add(-time.Hour)))
	if err != nil {
		t.Fatalf("Publish base entry: %v", err)
	}
	if published.Version != 1 || published.ID == "" || published.CreatedAt.IsZero() {
		t.Fatalf("unexpected published entry %+v", published)
	}

	got, err := svc.ResolveIntent(ctx, entA, scope, "生产异常状态")
	if err != nil {
		t.Fatalf("ResolveIntent: %v", err)
	}
	if len(got.DataNeeds) != 1 || got.DataNeeds[0].Domain != "mes" || got.DataNeeds[0].NeedKind != "production_anomaly_status" {
		t.Fatalf("expected mes/production_anomaly_status data need, got %+v", got.DataNeeds)
	}
	wantKeys := map[string]bool{"work_order_no": false, "production_line": false, "time_window": false}
	for _, k := range got.DataNeeds[0].BusinessKeys {
		wantKeys[k] = true
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Fatalf("expected business key %q in resolved data need", k)
		}
	}
	if err := model.NoConnectorLeak(got); err != nil {
		t.Fatalf("NoConnectorLeak on the resolved context must pass, got %v", err)
	}
	raw := fmt.Sprintf("%+v", got)
	for _, shape := range []string{"http://", "https://", "jdbc:", "sqlserver://", "SELECT ", "api/"} {
		if strings.Contains(raw, shape) {
			t.Fatalf("resolved OperatingContext leaks connector-shaped content %q: %s", shape, raw)
		}
	}

	// --- Mandated: conflicting authority rules across entries fail publish
	// with a typed error, and the DB persists nothing for the rejected call.
	// conflicting is a DIFFERENT entry (different intent_key) claiming a
	// different authority_tier for the SAME mes/production_anomaly_status
	// data need, in the same scope and an overlapping effective interval.
	conflicting := mesEntry(entA, scope, 1, now.Add(-time.Hour))
	conflicting.IntentKey = "mes.production_anomaly_status_v2"
	conflicting.IntentPhrases = []string{"生产异常状态第二版"}
	conflicting.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	conflicting.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	_, err = svc.Publish(ctx, conflicting)
	var conflict *RuleConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *RuleConflictError from a conflicting authority tier, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrRuleConflict) {
		t.Fatalf("expected errors.Is(err, ErrRuleConflict), got %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operating_map_entries WHERE enterprise_id=$1 AND org_scope=$2`, entA, scope).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("expected the conflicting publish to persist nothing, found %d rows", rowCount)
	}

	// --- Mandated: expired rule is excluded from resolution.
	expiringScope := "department:mes-floor-expiring"
	expiring := mesEntry(entA, expiringScope, 1, now.Add(-2*time.Hour))
	expiring.EffectiveTo = now.Add(-time.Hour)
	if _, err := svc.Publish(ctx, expiring); err != nil {
		t.Fatalf("Publish expiring entry: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, entA, expiringScope, "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected an expired entry to be excluded, got %v", err)
	}

	// --- Mandated: org-version change makes a stale entry unresolvable
	// until it is republished at the new org_version.
	orgVersionScope := "department:mes-floor-orgver"
	k1 := mesEntry(entA, orgVersionScope, 1, now.Add(-2*time.Hour))
	if _, err := svc.Publish(ctx, k1); err != nil {
		t.Fatalf("Publish K1: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, entA, orgVersionScope, "生产异常状态"); err != nil {
		t.Fatalf("expected K1 to resolve before any org_version bump, got %v", err)
	}
	k2 := lineStoppageEntry(entA, orgVersionScope, 2, now.Add(-time.Hour))
	if _, err := svc.Publish(ctx, k2); err != nil {
		t.Fatalf("Publish K2 at bumped org_version: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, entA, orgVersionScope, "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected K1 (org_version=1) to be excluded once K2 asserted org_version=2, got %v", err)
	}
	k1v2 := mesEntry(entA, orgVersionScope, 2, now.Add(-30*time.Minute))
	if _, err := svc.Publish(ctx, k1v2); err != nil {
		t.Fatalf("Publish K1 v2 at the new org_version: %v", err)
	}
	resolved, err := svc.ResolveIntent(ctx, entA, orgVersionScope, "生产异常状态")
	if err != nil {
		t.Fatalf("expected republished K1 at org_version=2 to resolve, got %v", err)
	}
	if resolved.OrganizationContext.OrgVersion != 2 {
		t.Fatalf("expected resolved org_version 2, got %d", resolved.OrganizationContext.OrgVersion)
	}

	// --- Mandated: tenant isolation. A different enterprise, and a
	// different (or merely prefix-overlapping) org_scope within the SAME
	// enterprise, must both see not-found — never another tenant's data.
	if _, err := svc.ResolveIntent(ctx, entB, scope, "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a different enterprise to see not-found, got %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, entA, "department:mes-floor-1-annex", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a sibling org_scope to see not-found, got %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, entA, "department:mes-floor", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a prefix of the published org_scope to see not-found, got %v", err)
	}

	// --- BLOCKER regression (Postgres leg): after K v1 (system_of_record,
	// open-ended) is superseded by K v2 (advisory), a neighbor asserting
	// system_of_record must deterministically FAIL naming v2, and a
	// neighbor agreeing with v2 must publish cleanly — regardless of
	// physical row order, which the old first-seen logic depended on.
	supersedeScope := "department:mes-floor-supersede"
	if _, err := svc.Publish(ctx, mesEntry(entA, supersedeScope, 1, now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("Publish K v1: %v", err)
	}
	sv2 := mesEntry(entA, supersedeScope, 1, now.Add(-time.Hour))
	sv2.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	sv2.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	governing, err := svc.Publish(ctx, sv2)
	if err != nil || governing.Version != 2 {
		t.Fatalf("supersession to v2 must publish: version=%d err=%v", governing.Version, err)
	}
	disagreeing := mesEntry(entA, supersedeScope, 1, now.Add(-time.Hour))
	disagreeing.IntentKey = "mes.production_anomaly_dashboard"
	disagreeing.IntentPhrases = []string{"生产异常看板"}
	_, err = svc.Publish(ctx, disagreeing)
	var supersedeConflict *RuleConflictError
	if !errors.As(err, &supersedeConflict) {
		t.Fatalf("expected *RuleConflictError against governing v2, got %T: %v", err, err)
	}
	if supersedeConflict.ConflictingEntryID != governing.ID || supersedeConflict.ConflictingValue != string(model.AuthorityAdvisory) {
		t.Fatalf("conflict must deterministically name governing v2 (advisory), got %+v", supersedeConflict)
	}
	agreeing := mesEntry(entA, supersedeScope, 1, now.Add(-time.Hour))
	agreeing.IntentKey = "mes.production_anomaly_digest"
	agreeing.IntentPhrases = []string{"生产异常摘要"}
	agreeing.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	agreeing.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	if _, err := svc.Publish(ctx, agreeing); err != nil {
		t.Fatalf("agreeing with governing v2 must publish cleanly (superseded v1 must not participate), got %v", err)
	}

	// --- Touching intervals at the exact boundary do not overlap: an entry
	// ending at T ([x, T)) and a candidate starting at T with directly
	// contradictory rules must both publish.
	touchingScope := "department:mes-floor-touching"
	boundary := now.Add(-time.Hour)
	early := mesEntry(entA, touchingScope, 1, now.Add(-2*time.Hour))
	early.EffectiveTo = boundary
	if _, err := svc.Publish(ctx, early); err != nil {
		t.Fatalf("Publish early interval: %v", err)
	}
	lateEntry := mesEntry(entA, touchingScope, 1, boundary)
	lateEntry.IntentKey = "mes.production_anomaly_dashboard"
	lateEntry.IntentPhrases = []string{"生产异常看板"}
	lateEntry.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	lateEntry.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	if _, err := svc.Publish(ctx, lateEntry); err != nil {
		t.Fatalf("touching intervals [x, T) and [T, ...) must not overlap or conflict, got %v", err)
	}

	// --- The conflict gate is tenant- and scope-scoped: directly
	// contradictory rules in ANOTHER enterprise (same org_scope) or in
	// another org_scope of the same enterprise must neither block publish
	// nor bleed into per-(enterprise, scope, intent_key) version numbering.
	crossTenant := mesEntry(entB, scope, 1, now.Add(-time.Hour))
	crossTenant.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	crossTenant.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	publishedB, err := svc.Publish(ctx, crossTenant)
	if err != nil || publishedB.Version != 1 {
		t.Fatalf("conflicting rule in another enterprise must not block publish or share version numbering: version=%d err=%v", publishedB.Version, err)
	}
	crossScope := mesEntry(entA, "department:mes-floor-elsewhere", 1, now.Add(-time.Hour))
	crossScope.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	crossScope.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	publishedElsewhere, err := svc.Publish(ctx, crossScope)
	if err != nil || publishedElsewhere.Version != 1 {
		t.Fatalf("conflicting rule in another org_scope must not block publish or share version numbering: version=%d err=%v", publishedElsewhere.Version, err)
	}

	// --- Operating Map entries are immutable, append-only history: a raw
	// UPDATE, DELETE or TRUNCATE must be rejected by the database triggers
	// (row-level for UPDATE/DELETE; statement-level for TRUNCATE, which
	// row-level triggers never see).
	if _, err := pool.Exec(ctx, `UPDATE operating_map_entries SET org_version=99 WHERE enterprise_id=$1 AND org_scope=$2`, entA, scope); err == nil {
		t.Fatalf("expected UPDATE on operating_map_entries to be rejected by the immutability trigger")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM operating_map_entries WHERE enterprise_id=$1 AND org_scope=$2`, entA, scope); err == nil {
		t.Fatalf("expected DELETE on operating_map_entries to be rejected by the immutability trigger")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE operating_map_entries`); err == nil {
		t.Fatalf("expected TRUNCATE on operating_map_entries to be rejected by the statement-level trigger")
	}

	// --- Migration 000015's down guard: published entries are historical
	// governance records, so downgrading while any exist must fail.
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.DownToContext(ctx, sqldb, "migrations", 14); err == nil {
		t.Fatalf("expected the down migration to refuse to drop operating_map_entries while rows exist")
	}
	var stillExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operating_map_entries WHERE enterprise_id=$1)`, entA).Scan(&stillExists); err != nil {
		t.Fatal(err)
	}
	if !stillExists {
		t.Fatalf("expected rows to remain after the refused down migration")
	}
}
