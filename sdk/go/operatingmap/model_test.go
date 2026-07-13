package operatingmap_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
)

// validMESEntry returns a well-formed Operating Map Entry publishing the
// "生产异常状态" (production anomaly status) business intent: pure business
// semantics — domain, need kind, business keys, authority tier, freshness —
// never a connector, endpoint or credential. CorrelationRules is left empty:
// a correlation needs at least two distinct data needs, and this fixture
// declares only one; tests that exercise correlation add a second need.
func validMESEntry() operatingmap.Entry {
	return operatingmap.Entry{
		EnterpriseID:         "ent-1",
		OrgScope:             "department:mes-floor-1",
		OrgVersion:           1,
		IntentKey:            "mes.production_anomaly_status",
		IntentPhrases:        []string{"生产异常状态", "production anomaly status"},
		EffectiveFrom:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		SourcePolicyRevision: "policy-rev-42",
		GovernanceReviewRef:  "gov-review-9001",
		DataNeeds: []operatingmap.DataNeed{{
			Domain:               "mes",
			NeedKind:             "production_anomaly_status",
			BusinessKeys:         []string{"work_order_no", "production_line", "time_window"},
			AuthorityTier:        operatingmap.AuthoritySystemOfRecord,
			FreshnessRequirement: "PT5M",
		}},
		MethodRefs: []operatingmap.MethodRef{
			{BusinessCapability: "mes.anomaly.read", Kind: "read"},
		},
		BusinessCapabilities: []operatingmap.BusinessCapability{
			{Name: "mes.anomaly.read", Description: "Read the current production anomaly status for a work order"},
		},
		AuthorityRules: []operatingmap.AuthorityRule{
			{Domain: "mes", NeedKind: "production_anomaly_status", AuthorityTier: operatingmap.AuthoritySystemOfRecord},
		},
		FreshnessRules: []operatingmap.FreshnessRule{
			{Domain: "mes", NeedKind: "production_anomaly_status", FreshnessRequirement: "PT5M"},
		},
		ConflictRules: []operatingmap.ConflictRule{
			{Domain: "mes", NeedKind: "production_anomaly_status", Resolution: operatingmap.ResolvePreferAuthorityTier},
		},
		MemoryRefs: []operatingmap.MemoryRef{
			{Handle: "mem-precedent-1", ContentHash: "sha256:deadbeefdeadbeefdeadbeefdeadbeef", Kind: "prior_resolution"},
		},
	}
}

func TestEntryValidateAcceptsSemanticMESProductionAnomalyEntry(t *testing.T) {
	e := validMESEntry()
	if err := e.Validate(); err != nil {
		t.Fatalf("expected a valid semantic MES entry, got %v", err)
	}
}

// TestEntryValidateProducesNoConnectorShapedStrings is the mandated proof
// that a resolved/published MES production-anomaly entry carries semantic
// business keys only — never a connector, endpoint, credential or raw
// query. It marshals the entry and scans for the exact shapes called out by
// the task: "http://", "https://", "jdbc:", "sqlserver://", "SELECT ",
// "api/".
func TestEntryValidateProducesNoConnectorShapedStrings(t *testing.T) {
	e := validMESEntry()
	if err := e.Validate(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(raw)
	for _, shape := range []string{"http://", "https://", "jdbc:", "sqlserver://", "SELECT ", "api/"} {
		if strings.Contains(text, shape) {
			t.Fatalf("marshaled entry leaks connector-shaped content %q: %s", shape, text)
		}
	}
	if err := operatingmap.NoConnectorLeak(e); err != nil {
		t.Fatalf("NoConnectorLeak on a pure-semantic entry must pass, got %v", err)
	}
	// The entry must actually carry the semantic MES data need and business
	// keys the task mandates, not merely be free of connector shapes.
	if len(e.DataNeeds) != 1 {
		t.Fatalf("expected exactly one data need, got %d", len(e.DataNeeds))
	}
	need := e.DataNeeds[0]
	if need.Domain != "mes" || need.NeedKind != "production_anomaly_status" {
		t.Fatalf("expected mes/production_anomaly_status data need, got %+v", need)
	}
	wantKeys := map[string]bool{"work_order_no": false, "production_line": false, "time_window": false}
	for _, k := range need.BusinessKeys {
		wantKeys[k] = true
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Fatalf("expected business key %q on the resolved data need", k)
		}
	}
	found := false
	for _, phrase := range e.IntentPhrases {
		if phrase == "生产异常状态" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the entry to register the Chinese intent phrase 生产异常状态")
	}
}

func TestNoConnectorLeakCatchesKnownConnectorShapes(t *testing.T) {
	cases := []struct{ name, probe string }{
		{"http scheme", "http://mes.internal.example.com/anomalies"},
		{"https scheme", "https://mes.internal.example.com/anomalies"},
		{"jdbc url", "jdbc:sqlserver://mes-db:1433;databaseName=prod"},
		{"sqlserver scheme", "sqlserver://mes-db:1433"},
		{"sql select-from", "SELECT status FROM work_orders"},
		{"api path segment", "api/v1/anomalies"},
		{"ado.net connection string", "Data Source=mes-db;Initial Catalog=prod;Integrated Security=SSPI"},
		{"connection string credentials", "Server=mes-db;Database=prod;User Id=svc;Password=hunter2;"},
		{"bare password assignment", "password = hunter2"},
		{"bearer authorization", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig"},
		{"unc share path", `\\mes-file01\exports\anomalies.csv`},
		{"multiline select-from", "SELECT\n  status\nFROM work_orders"},
		{"select star from", "SELECT*FROM work_orders"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := operatingmap.NoConnectorLeak(map[string]string{"value": c.probe}); err == nil {
				t.Fatalf("expected NoConnectorLeak to reject %q", c.probe)
			} else if !errors.Is(err, operatingmap.ErrConnectorShapedContent) {
				t.Fatalf("expected ErrConnectorShapedContent, got %v", err)
			}
		})
	}
}

// TestNoConnectorLeakAllowsBusinessLanguage pins the sharpened SQL and api
// patterns against the reviewer's clean probes: business phrasing that
// merely contains the word "select" (with no following "from"),
// identifiers containing "api_" (not an "api/" path segment), ISO-8601
// freshness durations and Chinese intent phrases must all pass clean.
func TestNoConnectorLeakAllowsBusinessLanguage(t *testing.T) {
	cases := []struct{ name, probe string }{
		{"select without from", "Select the current production line"},
		{"chinese intent phrase", "生产异常状态"},
		{"iso8601 freshness", "PT5M"},
		{"api underscore identifier", "api_gateway_status"},
		{"business key", "work_order_no"},
		{"business capability", "mes.anomaly.read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := operatingmap.NoConnectorLeak(map[string]string{"value": c.probe}); err != nil {
				t.Fatalf("expected %q to pass clean, got %v", c.probe, err)
			}
		})
	}
}

func TestEntryValidateRejectsConnectorShapedContent(t *testing.T) {
	cases := map[string]func(*operatingmap.Entry){
		"business key": func(e *operatingmap.Entry) {
			e.DataNeeds[0].BusinessKeys = append(e.DataNeeds[0].BusinessKeys, "https://mes.internal.example.com/api/work-orders")
		},
		"business capability description": func(e *operatingmap.Entry) {
			e.BusinessCapabilities[0].Description = "runs SELECT status FROM work_orders on the floor database"
		},
		"memory ref handle": func(e *operatingmap.Entry) {
			e.MemoryRefs[0].Handle = "jdbc:sqlserver://mes-db:1433"
		},
		"intent phrase": func(e *operatingmap.Entry) {
			e.IntentPhrases = append(e.IntentPhrases, "check api/v1/anomalies status")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := validMESEntry()
			mutate(&e)
			err := e.Validate()
			if err == nil {
				t.Fatalf("expected connector-shaped content to be rejected")
			}
			if !errors.Is(err, operatingmap.ErrConnectorShapedContent) {
				t.Fatalf("expected ErrConnectorShapedContent, got %v", err)
			}
		})
	}
}

func TestEntryValidateRejectsConflictingAuthorityRulesWithinOneEntry(t *testing.T) {
	e := validMESEntry()
	e.AuthorityRules = append(e.AuthorityRules, operatingmap.AuthorityRule{
		Domain: "mes", NeedKind: "production_anomaly_status", AuthorityTier: operatingmap.AuthorityAdvisory,
	})
	if err := e.Validate(); err == nil {
		t.Fatalf("expected two disagreeing authority rules for the same data need to be rejected")
	}
}

func TestEntryValidateRejectsConflictingFreshnessRulesWithinOneEntry(t *testing.T) {
	e := validMESEntry()
	e.FreshnessRules = append(e.FreshnessRules, operatingmap.FreshnessRule{
		Domain: "mes", NeedKind: "production_anomaly_status", FreshnessRequirement: "PT1H",
	})
	if err := e.Validate(); err == nil {
		t.Fatalf("expected two disagreeing freshness rules for the same data need to be rejected")
	}
}

func TestEntryValidateRejectsDataNeedAuthorityTierMismatchWithOwnRule(t *testing.T) {
	e := validMESEntry()
	e.DataNeeds[0].AuthorityTier = operatingmap.AuthorityAdvisory
	// AuthorityRules[0] still claims system_of_record for the same need.
	if err := e.Validate(); err == nil {
		t.Fatalf("expected a data_need whose authority_tier disagrees with its own authority_rule to be rejected")
	}
}

func TestEntryValidateRejectsRuleReferencingUnknownDataNeed(t *testing.T) {
	e := validMESEntry()
	e.AuthorityRules = append(e.AuthorityRules, operatingmap.AuthorityRule{
		Domain: "erp", NeedKind: "purchase_order_status", AuthorityTier: operatingmap.AuthoritySystemOfRecord,
	})
	if err := e.Validate(); err == nil {
		t.Fatalf("expected an authority_rule with no matching data_need to be rejected")
	}
}

func TestEntryValidateRejectsCorrelationRuleWithUndeclaredBusinessKey(t *testing.T) {
	e := validMESEntry()
	e.DataNeeds = append(e.DataNeeds, operatingmap.DataNeed{
		Domain: "erp", NeedKind: "purchase_order_status",
		BusinessKeys: []string{"purchase_order_no"}, AuthorityTier: operatingmap.AuthoritySystemOfRecord,
	})
	e.CorrelationRules = []operatingmap.CorrelationRule{{
		BusinessKey: "work_order_no",
		NeedKinds:   []string{"production_anomaly_status", "purchase_order_status"},
	}}
	if err := e.Validate(); err == nil {
		t.Fatalf("expected a correlation_rule keyed on a business key the erp need does not declare to be rejected")
	}
}

func TestEntryValidateAcceptsCrossDomainCorrelationRule(t *testing.T) {
	e := validMESEntry()
	e.DataNeeds = append(e.DataNeeds, operatingmap.DataNeed{
		Domain: "erp", NeedKind: "purchase_order_status",
		BusinessKeys: []string{"work_order_no", "purchase_order_no"}, AuthorityTier: operatingmap.AuthoritySystemOfRecord,
	})
	e.CorrelationRules = []operatingmap.CorrelationRule{{
		BusinessKey: "work_order_no",
		NeedKinds:   []string{"production_anomaly_status", "purchase_order_status"},
		Description: "join MES anomalies to ERP purchase orders by the shared work order number",
	}}
	if err := e.Validate(); err != nil {
		t.Fatalf("expected a well-formed cross-domain correlation rule to validate, got %v", err)
	}
}

func TestEntryValidateAcceptsOpenEndedEffectiveTo(t *testing.T) {
	e := validMESEntry()
	if !e.EffectiveTo.IsZero() {
		t.Fatalf("fixture must start open-ended for this test")
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("zero EffectiveTo (open-ended) must validate, got %v", err)
	}
}

func TestEntryValidateRejectsEffectiveToBeforeEffectiveFrom(t *testing.T) {
	e := validMESEntry()
	e.EffectiveTo = e.EffectiveFrom.Add(-time.Hour)
	if err := e.Validate(); err == nil {
		t.Fatalf("expected effective_to before effective_from to be rejected")
	}
}

func TestEntryValidateRejectsMissingRequiredFields(t *testing.T) {
	cases := map[string]func(*operatingmap.Entry){
		"enterprise_id":           func(e *operatingmap.Entry) { e.EnterpriseID = "" },
		"org_scope":               func(e *operatingmap.Entry) { e.OrgScope = "" },
		"intent_key":              func(e *operatingmap.Entry) { e.IntentKey = "" },
		"intent_key uppercase":    func(e *operatingmap.Entry) { e.IntentKey = "MES.Production_Anomaly_Status" },
		"intent_phrases empty":    func(e *operatingmap.Entry) { e.IntentPhrases = nil },
		"effective_from zero":     func(e *operatingmap.Entry) { e.EffectiveFrom = time.Time{} },
		"source_policy_revision":  func(e *operatingmap.Entry) { e.SourcePolicyRevision = "" },
		"governance_review_ref":   func(e *operatingmap.Entry) { e.GovernanceReviewRef = "" },
		"data_needs empty":        func(e *operatingmap.Entry) { e.DataNeeds = nil },
		"data_need business_keys": func(e *operatingmap.Entry) { e.DataNeeds[0].BusinessKeys = nil },
		"data_need authority":     func(e *operatingmap.Entry) { e.DataNeeds[0].AuthorityTier = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := validMESEntry()
			mutate(&e)
			if err := e.Validate(); err == nil {
				t.Fatalf("expected invalid %s to be rejected", name)
			}
		})
	}
}

func TestEntryJSONUsesSnakeCaseTags(t *testing.T) {
	e := validMESEntry()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "enterprise_id", "org_scope", "org_version", "intent_key", "intent_phrases",
		"version", "effective_from", "effective_to", "source_policy_revision",
		"governance_review_ref", "data_needs", "method_refs", "business_capabilities",
		"authority_rules", "freshness_rules", "conflict_rules", "memory_refs", "created_at",
	} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("expected JSON key %q in marshaled Entry, got keys %v", key, doc)
		}
	}
	needs, ok := doc["data_needs"].([]any)
	if !ok || len(needs) != 1 {
		t.Fatalf("expected one data_needs entry, got %v", doc["data_needs"])
	}
	need := needs[0].(map[string]any)
	for _, key := range []string{"domain", "need_kind", "business_keys", "authority_tier", "freshness_requirement"} {
		if _, ok := need[key]; !ok {
			t.Fatalf("expected JSON key %q on data_need, got %v", key, need)
		}
	}
}

func TestOrganizationContextJSONTags(t *testing.T) {
	oc := operatingmap.OrganizationContext{EnterpriseID: "ent-1", OrgScope: "department:mes-floor-1", OrgVersion: 3}
	raw, err := json.Marshal(oc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(raw)
	for _, want := range []string{`"enterprise_id":"ent-1"`, `"org_scope":"department:mes-floor-1"`, `"org_version":3`} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in %s", want, text)
		}
	}
}
