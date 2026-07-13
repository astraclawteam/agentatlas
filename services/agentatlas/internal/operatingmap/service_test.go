package operatingmap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
)

// memoryStore is a minimal in-process Store used only by this file's unit
// tests, so the Publish/ResolveIntent business logic in service.go can be
// exercised fast and without Postgres. Its interval and version semantics
// intentionally mirror postgres.go's (see the Store interface doc comments
// in service.go); postgres_integration_test.go proves the real store agrees.
type memoryStore struct {
	mu      sync.Mutex
	entries []model.Entry
}

func newMemoryStore() *memoryStore { return &memoryStore{} }

func entryOpenTo(e model.Entry) time.Time {
	if e.EffectiveTo.IsZero() {
		return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return e.EffectiveTo
}

func (m *memoryStore) Publish(_ context.Context, candidate model.Entry, check func([]model.Entry) error) (model.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var overlapping []model.Entry
	var maxVersion int32
	candidateTo := entryOpenTo(candidate)
	for _, e := range m.entries {
		if e.EnterpriseID != candidate.EnterpriseID || e.OrgScope != candidate.OrgScope {
			continue
		}
		if e.IntentKey == candidate.IntentKey && e.Version > maxVersion {
			maxVersion = e.Version
		}
		if e.EffectiveFrom.Before(candidateTo) && candidate.EffectiveFrom.Before(entryOpenTo(e)) {
			overlapping = append(overlapping, e)
		}
	}
	if check != nil {
		if err := check(overlapping); err != nil {
			return model.Entry{}, err
		}
	}
	candidate.Version = maxVersion + 1
	candidate.ID = fmt.Sprintf("opm_%s_%s_v%d", candidate.OrgScope, candidate.IntentKey, candidate.Version)
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}
	m.entries = append(m.entries, candidate)
	return candidate, nil
}

func (m *memoryStore) ActiveEntries(_ context.Context, enterpriseID, orgScope string, asOf time.Time) ([]model.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Entry
	for _, e := range m.entries {
		if e.EnterpriseID != enterpriseID || e.OrgScope != orgScope {
			continue
		}
		if e.EffectiveFrom.After(asOf) {
			continue
		}
		if !e.EffectiveTo.IsZero() && !e.EffectiveTo.After(asOf) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// mesEntry returns a ready-to-publish Entry for the "生产异常状态"
// (production anomaly status) business intent: pure business semantics
// bound to enterprise governance, never a connector or credential.
func mesEntry(enterpriseID, orgScope string, orgVersion int64, effectiveFrom time.Time) model.Entry {
	return model.Entry{
		EnterpriseID:         enterpriseID,
		OrgScope:             orgScope,
		OrgVersion:           orgVersion,
		IntentKey:            "mes.production_anomaly_status",
		IntentPhrases:        []string{"生产异常状态", "production anomaly status"},
		EffectiveFrom:        effectiveFrom,
		SourcePolicyRevision: "policy-rev-1",
		GovernanceReviewRef:  "gov-review-1",
		DataNeeds: []model.DataNeed{{
			Domain: "mes", NeedKind: "production_anomaly_status",
			BusinessKeys:  []string{"work_order_no", "production_line", "time_window"},
			AuthorityTier: model.AuthoritySystemOfRecord, FreshnessRequirement: "PT5M",
		}},
		MethodRefs:           []model.MethodRef{{BusinessCapability: "mes.anomaly.read", Kind: "read"}},
		BusinessCapabilities: []model.BusinessCapability{{Name: "mes.anomaly.read", Description: "Read current production anomaly status"}},
		AuthorityRules:       []model.AuthorityRule{{Domain: "mes", NeedKind: "production_anomaly_status", AuthorityTier: model.AuthoritySystemOfRecord}},
		FreshnessRules:       []model.FreshnessRule{{Domain: "mes", NeedKind: "production_anomaly_status", FreshnessRequirement: "PT5M"}},
	}
}

// lineStoppageEntry returns a ready-to-publish Entry for a DIFFERENT
// business need (domain "mes", need_kind "line_stoppage_report") than
// mesEntry's production_anomaly_status. Tests use it to publish a second,
// unrelated entry — e.g. to advance a scope's org_version — without ever
// touching production_anomaly_status's rules, so any interaction those
// tests observe cannot be mistaken for coincidental rule-value matching.
func lineStoppageEntry(enterpriseID, orgScope string, orgVersion int64, effectiveFrom time.Time) model.Entry {
	return model.Entry{
		EnterpriseID:         enterpriseID,
		OrgScope:             orgScope,
		OrgVersion:           orgVersion,
		IntentKey:            "mes.line_stoppage_report",
		IntentPhrases:        []string{"停线报告", "line stoppage report"},
		EffectiveFrom:        effectiveFrom,
		SourcePolicyRevision: "policy-rev-1",
		GovernanceReviewRef:  "gov-review-1",
		DataNeeds: []model.DataNeed{{
			Domain: "mes", NeedKind: "line_stoppage_report",
			BusinessKeys:  []string{"production_line", "time_window"},
			AuthorityTier: model.AuthoritySystemOfRecord, FreshnessRequirement: "PT15M",
		}},
		MethodRefs:           []model.MethodRef{{BusinessCapability: "mes.stoppage.read", Kind: "read"}},
		BusinessCapabilities: []model.BusinessCapability{{Name: "mes.stoppage.read", Description: "Read current line stoppage report"}},
		AuthorityRules:       []model.AuthorityRule{{Domain: "mes", NeedKind: "line_stoppage_report", AuthorityTier: model.AuthoritySystemOfRecord}},
		FreshnessRules:       []model.FreshnessRule{{Domain: "mes", NeedKind: "line_stoppage_report", FreshnessRequirement: "PT15M"}},
	}
}

func newTestService(t *testing.T, store Store, now time.Time) *Service {
	t.Helper()
	svc, err := NewService(store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestResolveIntentReturnsSemanticMESDataNeedForChineseProductionAnomalyIntent
// is the mandated proof: the Chinese employee intent "生产异常状态" resolves
// to a semantic MES data need and business keys, and the resolved context
// contains no connector/API/URL/credential-shaped strings.
func TestResolveIntentReturnsSemanticMESDataNeedForChineseProductionAnomalyIntent(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	published, err := svc.Publish(ctx, mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour)))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Version != 1 {
		t.Fatalf("expected first publish to be version 1, got %d", published.Version)
	}

	got, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态")
	if err != nil {
		t.Fatalf("ResolveIntent: %v", err)
	}
	if len(got.DataNeeds) != 1 || got.DataNeeds[0].Domain != "mes" || got.DataNeeds[0].NeedKind != "production_anomaly_status" {
		t.Fatalf("expected one mes/production_anomaly_status data need, got %+v", got.DataNeeds)
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
	if len(got.BusinessCapabilities) != 1 || got.BusinessCapabilities[0].Name != "mes.anomaly.read" {
		t.Fatalf("expected business capability mes.anomaly.read, got %+v", got.BusinessCapabilities)
	}
	if got.OrganizationContext.EnterpriseID != "ent-1" || got.OrganizationContext.OrgScope != "department:mes-floor-1" {
		t.Fatalf("unexpected organization context %+v", got.OrganizationContext)
	}

	raw := fmt.Sprintf("%+v", got)
	for _, shape := range []string{"http://", "https://", "jdbc:", "sqlserver://", "SELECT ", "api/"} {
		if strings.Contains(raw, shape) {
			t.Fatalf("resolved OperatingContext leaks connector-shaped content %q: %s", shape, raw)
		}
	}
	if err := model.NoConnectorLeak(got); err != nil {
		t.Fatalf("NoConnectorLeak on the resolved context must pass, got %v", err)
	}
}

func TestResolveIntentRejectsInvalidRequest(t *testing.T) {
	svc := newTestService(t, newMemoryStore(), time.Now())
	ctx := context.Background()
	cases := []struct{ enterpriseID, orgScope, intent string }{
		{"", "department:mes-floor-1", "生产异常状态"},
		{"ent-1", "", "生产异常状态"},
		{"ent-1", "department:mes-floor-1", ""},
	}
	for _, c := range cases {
		if _, err := svc.ResolveIntent(ctx, c.enterpriseID, c.orgScope, c.intent); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %+v: expected ErrInvalidRequest, got %v", c, err)
		}
	}
}

func TestResolveIntentReturnsErrWhenNothingPublished(t *testing.T) {
	svc := newTestService(t, newMemoryStore(), time.Now())
	if _, err := svc.ResolveIntent(context.Background(), "ent-1", "department:mes-floor-1", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected ErrIntentNotResolved, got %v", err)
	}
}

// TestPublishRejectsConflictingAuthorityTierAcrossEntries is the second
// mandated failing test: two entries claiming different authority tiers for
// the same data need in the same scope and overlapping interval must fail
// publication with a typed error.
func TestPublishRejectsConflictingAuthorityTierAcrossEntries(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	first := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	first.IntentKey = "mes.production_anomaly_status"
	if _, err := svc.Publish(ctx, first); err != nil {
		t.Fatalf("Publish first: %v", err)
	}

	second := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	second.IntentKey = "mes.line_stoppage_report" // different intent, same data need
	second.IntentPhrases = []string{"停线报告"}
	second.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	second.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory

	_, err := svc.Publish(ctx, second)
	if err == nil {
		t.Fatalf("expected conflicting authority tier to fail publication")
	}
	var conflict *RuleConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *RuleConflictError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrRuleConflict) {
		t.Fatalf("expected errors.Is(err, ErrRuleConflict), got %v", err)
	}
	if conflict.RuleType != "authority_tier" {
		t.Fatalf("expected authority_tier conflict, got %+v", conflict)
	}

	// The rejected entry must not have been persisted.
	active, err := store.ActiveEntries(ctx, "ent-1", "department:mes-floor-1", now)
	if err != nil {
		t.Fatalf("ActiveEntries: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected only the first entry to be persisted, got %d entries", len(active))
	}
}

// TestPublishRejectsConflictingFreshnessWindowAcrossEntries is the freshness
// half of the mandated conflicting-rules test.
func TestPublishRejectsConflictingFreshnessWindowAcrossEntries(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	first := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	if _, err := svc.Publish(ctx, first); err != nil {
		t.Fatalf("Publish first: %v", err)
	}

	second := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	second.IntentKey = "mes.line_stoppage_report"
	second.IntentPhrases = []string{"停线报告"}
	second.FreshnessRules[0].FreshnessRequirement = "PT1H"
	second.DataNeeds[0].FreshnessRequirement = "PT1H"

	_, err := svc.Publish(ctx, second)
	var conflict *RuleConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *RuleConflictError, got %T: %v", err, err)
	}
	if conflict.RuleType != "freshness_requirement" {
		t.Fatalf("expected freshness_requirement conflict, got %+v", conflict)
	}
}

// TestPublishAllowsNewVersionOfSameIntentKeyToChangeAuthorityTier proves
// supersession (republishing the SAME intent_key with a different rule) is
// never treated as a conflict, only disagreement between DIFFERENT entries
// is.
func TestPublishAllowsNewVersionOfSameIntentKeyToChangeAuthorityTier(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	svc := newTestService(t, newMemoryStore(), now)
	ctx := context.Background()
	first := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	if _, err := svc.Publish(ctx, first); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}
	second := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Minute))
	second.AuthorityRules[0].AuthorityTier = model.AuthorityDerived
	second.DataNeeds[0].AuthorityTier = model.AuthorityDerived
	published, err := svc.Publish(ctx, second)
	if err != nil {
		t.Fatalf("expected republishing the same intent_key to succeed, got %v", err)
	}
	if published.Version != 2 {
		t.Fatalf("expected version 2, got %d", published.Version)
	}
}

func TestPublishRejectsInvalidEntry(t *testing.T) {
	svc := newTestService(t, newMemoryStore(), time.Now())
	invalid := mesEntry("ent-1", "department:mes-floor-1", 1, time.Now())
	invalid.DataNeeds = nil
	if _, err := svc.Publish(context.Background(), invalid); err == nil {
		t.Fatalf("expected an entry with no data needs to be rejected")
	}
}

func TestPublishRejectsConnectorShapedContent(t *testing.T) {
	svc := newTestService(t, newMemoryStore(), time.Now())
	tainted := mesEntry("ent-1", "department:mes-floor-1", 1, time.Now())
	tainted.DataNeeds[0].BusinessKeys = append(tainted.DataNeeds[0].BusinessKeys, "https://mes.internal.example.com/anomalies")
	_, err := svc.Publish(context.Background(), tainted)
	if !errors.Is(err, model.ErrConnectorShapedContent) {
		t.Fatalf("expected model.ErrConnectorShapedContent, got %v", err)
	}
}

// TestResolveIntentExcludesExpiredEntry is the mandated "expired rule"
// scenario: an entry whose effective interval has passed must not resolve.
func TestResolveIntentExcludesExpiredEntry(t *testing.T) {
	published := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, published)
	ctx := context.Background()
	entry := mesEntry("ent-1", "department:mes-floor-1", 1, published)
	entry.EffectiveTo = published.Add(time.Hour)
	if _, err := svc.Publish(ctx, entry); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	beforeExpiry := newTestService(t, store, published.Add(30*time.Minute))
	if _, err := beforeExpiry.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态"); err != nil {
		t.Fatalf("expected resolution before expiry to succeed, got %v", err)
	}

	afterExpiry := newTestService(t, store, published.Add(2*time.Hour))
	if _, err := afterExpiry.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected an expired entry to be excluded (ErrIntentNotResolved), got %v", err)
	}
}

// TestResolveIntentExcludesEntryBoundToStaleOrgVersion is the mandated
// "org-version change" scenario: once a newer org_version has been asserted
// for the scope (by any published entry), an older entry still bound to the
// prior org_version is excluded until it is republished at the new
// org_version.
func TestResolveIntentExcludesEntryBoundToStaleOrgVersion(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()

	k1 := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-2*time.Hour))
	if _, err := svc.Publish(ctx, k1); err != nil {
		t.Fatalf("Publish K1: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态"); err != nil {
		t.Fatalf("expected K1 to resolve before any org_version bump, got %v", err)
	}

	k2 := lineStoppageEntry("ent-1", "department:mes-floor-1", 2, now.Add(-time.Hour))
	if _, err := svc.Publish(ctx, k2); err != nil {
		t.Fatalf("Publish K2 at bumped org_version: %v", err)
	}

	if _, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected K1 (org_version=1) to be excluded once K2 asserted org_version=2, got %v", err)
	}

	k1v2 := mesEntry("ent-1", "department:mes-floor-1", 2, now.Add(-30*time.Minute))
	if _, err := svc.Publish(ctx, k1v2); err != nil {
		t.Fatalf("Publish K1 v2 at the new org_version: %v", err)
	}
	got, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态")
	if err != nil {
		t.Fatalf("expected republished K1 at org_version=2 to resolve, got %v", err)
	}
	if got.OrganizationContext.OrgVersion != 2 {
		t.Fatalf("expected resolved org_version 2, got %d", got.OrganizationContext.OrgVersion)
	}
}

// TestResolveIntentEnforcesTenantIsolationAcrossEnterprise and
// TestResolveIntentEnforcesExactOrgScopeIsolation are the mandated
// tenant-isolation scenarios: a cross-tenant or cross-scope read must look
// like not-found, never leak that something is published elsewhere.
func TestResolveIntentEnforcesTenantIsolationAcrossEnterprise(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	if _, err := svc.Publish(ctx, mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-1", "生产异常状态"); err != nil {
		t.Fatalf("expected the publishing tenant to resolve, got %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, "ent-2", "department:mes-floor-1", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a different enterprise_id to see not-found, got %v", err)
	}
}

func TestResolveIntentEnforcesExactOrgScopeIsolation(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	if _, err := svc.Publish(ctx, mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor-2", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a sibling org_scope to see not-found (exact scope match only), got %v", err)
	}
	if _, err := svc.ResolveIntent(ctx, "ent-1", "department:mes-floor", "生产异常状态"); !errors.Is(err, ErrIntentNotResolved) {
		t.Fatalf("expected a prefix of the published org_scope to see not-found (no prefix matching), got %v", err)
	}
}

func TestPublishAssignsIncrementingVersionsPerIntentKey(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	svc := newTestService(t, newMemoryStore(), now)
	ctx := context.Background()
	var lastVersion int32
	for i := 0; i < 3; i++ {
		entry := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(time.Duration(i)*time.Minute-time.Hour))
		published, err := svc.Publish(ctx, entry)
		if err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
		if published.Version != lastVersion+1 {
			t.Fatalf("Publish #%d: expected version %d, got %d", i, lastVersion+1, published.Version)
		}
		lastVersion = published.Version
	}
}

// TestDetectRuleConflictsDedupesToLatestVersionAndIsOrderIndependent is the
// direct regression for the review BLOCKER against the shared, pure
// algorithm: after K v1 (system_of_record) is superseded by K v2
// (advisory) — both open-ended, so both rows stay "effective" forever — a
// neighbor's fate must depend only on the GOVERNING v2, in either store
// row order (the reviewer empirically produced both verdicts under the
// old first-seen logic by permuting row order).
func TestDetectRuleConflictsDedupesToLatestVersionAndIsOrderIndependent(t *testing.T) {
	rule := func(tier model.AuthorityTier) []model.AuthorityRule {
		return []model.AuthorityRule{{Domain: "mes", NeedKind: "production_anomaly_status", AuthorityTier: tier}}
	}
	v1 := model.Entry{ID: "opm_k_v1", IntentKey: "mes.production_anomaly_status", Version: 1, AuthorityRules: rule(model.AuthoritySystemOfRecord)}
	v2 := model.Entry{ID: "opm_k_v2", IntentKey: "mes.production_anomaly_status", Version: 2, AuthorityRules: rule(model.AuthorityAdvisory)}
	disagreeing := model.Entry{IntentKey: "mes.production_anomaly_dashboard", AuthorityRules: rule(model.AuthoritySystemOfRecord)}
	agreeing := model.Entry{IntentKey: "mes.production_anomaly_digest", AuthorityRules: rule(model.AuthorityAdvisory)}
	for name, overlapping := range map[string][]model.Entry{
		"superseded v1 first": {v1, v2},
		"governing v2 first":  {v2, v1},
	} {
		err := detectRuleConflicts(disagreeing, overlapping)
		var conflict *RuleConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("%s: expected *RuleConflictError against governing v2, got %v", name, err)
		}
		if conflict.ConflictingEntryID != "opm_k_v2" || conflict.ConflictingValue != string(model.AuthorityAdvisory) {
			t.Fatalf("%s: conflict must name superseding v2 (advisory), got %+v", name, conflict)
		}
		if err := detectRuleConflicts(agreeing, overlapping); err != nil {
			t.Fatalf("%s: agreeing with governing v2 must pass — superseded v1 must not participate, got %v", name, err)
		}
	}
}

// TestPublishAgainstSupersededOpenEndedVersionUsesGoverningVersion is the
// end-to-end BLOCKER regression through Service.Publish and a Store:
// publish K v1 (system_of_record, open-ended EffectiveTo), supersede with
// K v2 (advisory — allowed supersession), then a neighbor asserting
// system_of_record must deterministically FAIL naming v2, and a neighbor
// agreeing with v2 (advisory) must publish cleanly, proving the superseded
// v1 no longer participates in the gate at all.
func TestPublishAgainstSupersededOpenEndedVersionUsesGoverningVersion(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	svc := newTestService(t, store, now)
	ctx := context.Background()
	if _, err := svc.Publish(ctx, mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("Publish K v1: %v", err)
	}
	k2 := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	k2.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	k2.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	governing, err := svc.Publish(ctx, k2)
	if err != nil || governing.Version != 2 {
		t.Fatalf("supersession to v2 must publish: version=%d err=%v", governing.Version, err)
	}

	disagreeing := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	disagreeing.IntentKey = "mes.production_anomaly_dashboard"
	disagreeing.IntentPhrases = []string{"生产异常看板"}
	_, err = svc.Publish(ctx, disagreeing)
	var conflict *RuleConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *RuleConflictError, got %T: %v", err, err)
	}
	if conflict.ConflictingEntryID != governing.ID || conflict.ConflictingIntentKey != governing.IntentKey || conflict.ConflictingValue != string(model.AuthorityAdvisory) {
		t.Fatalf("conflict must deterministically name governing v2, got %+v", conflict)
	}

	agreeing := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))
	agreeing.IntentKey = "mes.production_anomaly_digest"
	agreeing.IntentPhrases = []string{"生产异常摘要"}
	agreeing.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	agreeing.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	if _, err := svc.Publish(ctx, agreeing); err != nil {
		t.Fatalf("agreeing with governing v2 must publish cleanly, got %v", err)
	}
}

// TestPublishTouchingIntervalsDoNotConflict pins the half-open interval
// semantics at the exact boundary: an entry whose EffectiveTo equals the
// candidate's EffectiveFrom does not overlap it, so even directly
// contradictory rules publish cleanly across the boundary instant.
func TestPublishTouchingIntervalsDoNotConflict(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	boundary := now.Add(-time.Hour)
	svc := newTestService(t, newMemoryStore(), now)
	ctx := context.Background()
	early := mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-2*time.Hour))
	early.EffectiveTo = boundary
	if _, err := svc.Publish(ctx, early); err != nil {
		t.Fatalf("Publish early interval: %v", err)
	}
	late := mesEntry("ent-1", "department:mes-floor-1", 1, boundary)
	late.IntentKey = "mes.production_anomaly_dashboard"
	late.IntentPhrases = []string{"生产异常看板"}
	late.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	late.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	if _, err := svc.Publish(ctx, late); err != nil {
		t.Fatalf("touching intervals [x, T) and [T, ...) must not overlap or conflict, got %v", err)
	}
}

// TestPublishConflictCheckIsTenantAndScopeScoped guards the enterprise_id
// and org_scope filters in the overlap and version queries: directly
// contradictory rules in a DIFFERENT enterprise, or in a different
// org_scope of the same enterprise, must neither block publication nor
// bleed into version numbering.
func TestPublishConflictCheckIsTenantAndScopeScoped(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	svc := newTestService(t, newMemoryStore(), now)
	ctx := context.Background()
	if _, err := svc.Publish(ctx, mesEntry("ent-1", "department:mes-floor-1", 1, now.Add(-time.Hour))); err != nil {
		t.Fatalf("Publish base entry: %v", err)
	}

	otherTenant := mesEntry("ent-2", "department:mes-floor-1", 1, now.Add(-time.Hour))
	otherTenant.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	otherTenant.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	published, err := svc.Publish(ctx, otherTenant)
	if err != nil || published.Version != 1 {
		t.Fatalf("conflicting rule in another enterprise must not block publish or share version numbering: version=%d err=%v", published.Version, err)
	}

	otherScope := mesEntry("ent-1", "department:mes-floor-2", 1, now.Add(-time.Hour))
	otherScope.AuthorityRules[0].AuthorityTier = model.AuthorityAdvisory
	otherScope.DataNeeds[0].AuthorityTier = model.AuthorityAdvisory
	published, err = svc.Publish(ctx, otherScope)
	if err != nil || published.Version != 1 {
		t.Fatalf("conflicting rule in another org_scope must not block publish or share version numbering: version=%d err=%v", published.Version, err)
	}
}
