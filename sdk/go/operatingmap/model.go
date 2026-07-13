// Package operatingmap is the enterprise Operating Map: cognition of WHERE
// and HOW to work for a business intent, never a connector catalog. Entry
// bundles nine versioned content types — OrganizationContext, MethodRef,
// DataNeed, BusinessCapability, CorrelationRule, AuthorityRule,
// FreshnessRule, ConflictRule and MemoryRef — expressed entirely in
// business semantics (domain, need kind, business keys, authority tier,
// freshness requirement, business capability name). Nothing in this
// package may carry a connector endpoint, credential or raw request body;
// NoConnectorLeak enforces that boundary and Entry.Validate calls it on
// every entry. Authoritative persistence, publication governance and
// intent resolution live in
// services/agentatlas/internal/operatingmap (Task 0C); this package is
// self-contained (stdlib only) so it can be imported by any consumer
// without pulling in service internals.
package operatingmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// AuthorityTier ranks how authoritative a DataNeed's source is.
type AuthorityTier string

const (
	AuthoritySystemOfRecord AuthorityTier = "system_of_record"
	AuthorityDerived        AuthorityTier = "derived"
	AuthorityAdvisory       AuthorityTier = "advisory"
)

// ConflictResolution names how a ConflictRule resolves disagreement between
// multiple qualifying sources for the same DataNeed at operating time —
// never how to query them.
type ConflictResolution string

const (
	ResolvePreferAuthorityTier ConflictResolution = "prefer_authority_tier"
	ResolvePreferFreshest      ConflictResolution = "prefer_freshest"
	ResolveEscalateHuman       ConflictResolution = "escalate_human"
)

// OrganizationContext identifies the enterprise, org scope and org version
// an Operating Map Entry or a resolved OperatingContext is bound to.
type OrganizationContext struct {
	EnterpriseID string `json:"enterprise_id"`
	OrgScope     string `json:"org_scope"`
	OrgVersion   int64  `json:"org_version"`
}

// DataNeed expresses BUSINESS semantics only: a business domain (e.g.
// "mes"), a need kind (e.g. "production_anomaly_status"), the business
// keys that identify one instance of the need (e.g. work order number,
// production line, time window), the authority tier of its source and an
// optional freshness requirement. It never carries a URL, endpoint, SQL
// statement, API path or credential — see NoConnectorLeak.
type DataNeed struct {
	Domain               string        `json:"domain"`
	NeedKind             string        `json:"need_kind"`
	BusinessKeys         []string      `json:"business_keys"`
	AuthorityTier        AuthorityTier `json:"authority_tier"`
	FreshnessRequirement string        `json:"freshness_requirement,omitempty"`
}

// BusinessCapability names what can be done in business language (e.g.
// "mes.anomaly.read", the capability sdk/go/workcase's ActionSpec
// fixtures dispatch against) — never an endpoint or provider SDK call.
type BusinessCapability struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// MethodRef names a BusinessCapability that can service a DataNeed. Kind is
// an open, business-defined verb (e.g. "read", "write"), mirroring
// sdk/go/workcase.ActionSpec.Kind.
type MethodRef struct {
	BusinessCapability string `json:"business_capability"`
	Kind               string `json:"kind"`
}

// CorrelationRule declares that BusinessKey correlates (joins) two or more
// data needs — potentially across business domains, e.g. an MES anomaly
// and an ERP purchase order sharing a work order number.
type CorrelationRule struct {
	BusinessKey string   `json:"business_key"`
	NeedKinds   []string `json:"need_kinds"`
	Description string   `json:"description,omitempty"`
}

// AuthorityRule declares the authority tier for a (Domain, NeedKind) pair.
// Two AuthorityRules for the same pair published under DIFFERENT entries in
// the same scope and an overlapping effective interval, naming different
// tiers, must fail publication (see the internal Service.Publish).
type AuthorityRule struct {
	Domain        string        `json:"domain"`
	NeedKind      string        `json:"need_kind"`
	AuthorityTier AuthorityTier `json:"authority_tier"`
}

// FreshnessRule declares the freshness requirement for a (Domain, NeedKind)
// pair, as an ISO-8601 duration (e.g. "PT5M"). Two FreshnessRules for the
// same pair published under DIFFERENT entries in the same scope and an
// overlapping effective interval, naming different windows, must fail
// publication (see the internal Service.Publish).
type FreshnessRule struct {
	Domain               string `json:"domain"`
	NeedKind             string `json:"need_kind"`
	FreshnessRequirement string `json:"freshness_requirement"`
}

// ConflictRule declares how to resolve disagreement between multiple
// qualifying sources for the same (Domain, NeedKind) DataNeed at operating
// time. This is distinct from the AuthorityRule/FreshnessRule PUBLICATION
// conflict above: a ConflictRule is published business content (what an
// agent should do when two sources disagree at read time); an
// AuthorityRule/FreshnessRule disagreement is a governance defect that
// blocks publication before it ever reaches an agent.
type ConflictRule struct {
	Domain     string             `json:"domain"`
	NeedKind   string             `json:"need_kind"`
	Resolution ConflictResolution `json:"resolution"`
}

// MemoryRef is an opaque, content-addressed pointer into institutional
// memory that grounds an Entry (e.g. a prior confirmed resolution or
// governance precedent). It carries a handle and hash only, never raw
// content — mirroring sdk/go/workcase.EvidenceRef.
type MemoryRef struct {
	Handle      string `json:"handle"`
	ContentHash string `json:"content_hash"`
	Kind        string `json:"kind,omitempty"`
}

// Entry is one immutable, versioned publication of Operating Map content:
// enterprise cognition of WHERE and HOW to work for one business intent,
// bound to (EnterpriseID, OrgScope, OrgVersion), an effective interval, a
// source governance policy revision and a governance review. Publishing a
// new Entry for the same (EnterpriseID, OrgScope, IntentKey) supersedes the
// prior version; Entry itself is never mutated in place.
//
// ID, Version and CreatedAt are assigned by the publishing service, not the
// caller; Validate does not check them.
type Entry struct {
	ID            string    `json:"id"`
	EnterpriseID  string    `json:"enterprise_id"`
	OrgScope      string    `json:"org_scope"`
	OrgVersion    int64     `json:"org_version"`
	IntentKey     string    `json:"intent_key"`
	IntentPhrases []string  `json:"intent_phrases"`
	Version       int32     `json:"version"`
	EffectiveFrom time.Time `json:"effective_from"`
	// EffectiveTo is always present on the wire (a time.Time struct is
	// never "empty", so omitempty would be a silent no-op — see
	// sdk/go/workcase.ActionSpec.ExpiresAt for the same convention). The
	// zero timestamp means "open-ended": no known end to this version's
	// effective interval.
	EffectiveTo          time.Time `json:"effective_to"`
	SourcePolicyRevision string    `json:"source_policy_revision"`
	GovernanceReviewRef  string    `json:"governance_review_ref"`

	DataNeeds            []DataNeed           `json:"data_needs"`
	MethodRefs           []MethodRef          `json:"method_refs,omitempty"`
	BusinessCapabilities []BusinessCapability `json:"business_capabilities,omitempty"`
	CorrelationRules     []CorrelationRule    `json:"correlation_rules,omitempty"`
	AuthorityRules       []AuthorityRule      `json:"authority_rules,omitempty"`
	FreshnessRules       []FreshnessRule      `json:"freshness_rules,omitempty"`
	ConflictRules        []ConflictRule       `json:"conflict_rules,omitempty"`
	MemoryRefs           []MemoryRef          `json:"memory_refs,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// OperatingContext is the result of resolving an employee intent against
// published Operating Map entries: matched MethodRefs, DataNeeds,
// BusinessCapabilities and applicable rules, bundled with the
// OrganizationContext they were resolved against. It returns business
// semantics only; it never contains credentials, connector URLs or
// provider-specific request bodies (see NoConnectorLeak).
type OperatingContext struct {
	Intent               string               `json:"intent"`
	OrganizationContext  OrganizationContext  `json:"organization_context"`
	DataNeeds            []DataNeed           `json:"data_needs,omitempty"`
	MethodRefs           []MethodRef          `json:"method_refs,omitempty"`
	BusinessCapabilities []BusinessCapability `json:"business_capabilities,omitempty"`
	CorrelationRules     []CorrelationRule    `json:"correlation_rules,omitempty"`
	AuthorityRules       []AuthorityRule      `json:"authority_rules,omitempty"`
	FreshnessRules       []FreshnessRule      `json:"freshness_rules,omitempty"`
	ConflictRules        []ConflictRule       `json:"conflict_rules,omitempty"`
	MemoryRefs           []MemoryRef          `json:"memory_refs,omitempty"`
}

// ErrConnectorShapedContent is returned by NoConnectorLeak (and therefore by
// Entry.Validate) when marshaled content matches one of the forbidden
// connector, credential or raw-query shapes enumerated on
// connectorShapePattern: URL schemes, "jdbc:" URLs, SQL SELECT...FROM
// shapes, "api/" path segments, connection-string credential keys,
// "Authorization: Bearer" values, and backslash-delimited share paths.
var ErrConnectorShapedContent = errors.New("operatingmap: content matches a forbidden connector, credential or raw-query shape (URL scheme, jdbc: URL, SQL SELECT...FROM, api/ path segment, connection-string credential key, bearer authorization, or backslash share path); the operating map carries business semantics only")

// connectorShapePattern enumerates exactly what NoConnectorLeak detects in
// marshaled content, case-insensitively:
//
//	shape                                    example that trips it
//	---------------------------------------  --------------------------------
//	any URL scheme "x://"                    http://, https://, sqlserver://
//	"jdbc:" connection-URL prefix            jdbc:sqlserver://mes-db:1433
//	SQL query shape: "select" then "from"    SELECT status FROM work_orders,
//	  within 300 chars, or "select*from"     SELECT\n...FROM x, SELECT*FROM x
//	"api/" path segment                      api/v1/anomalies
//	connection-string credential keys        password=, user id=, uid=, pwd=,
//	  ("<key>=", optional spaces)            data source=, initial catalog=,
//	                                         integrated security=
//	bearer authorization header value        Authorization: Bearer eyJ...
//	backslash-delimited share/drive path     \\mes-file01\exports, C:\dir\file
//	  (JSON escaping doubles each
//	  backslash; the pattern matches the
//	  escaped form)
//
// The scan runs over MARSHALED JSON, where a newline/tab is the two-byte
// escape `\n`/`\t` — a word character 'n'/'t' that swallows the `\b`
// boundary a naive `\bfrom\b` would need. The select...from alternative
// therefore accepts either a real word boundary or a JSON whitespace
// escape (`\n`, `\r`, `\t`) on the word edges, so a multi-line SQL
// statement is still caught after marshaling.
//
// Known, accepted fail-closed false positive: an English sentence that
// happens to contain the word "select" followed by the word "from" within
// 300 characters (for example "select the anomalies from the morning
// shift") is rejected even though it is not SQL. Business phrasing without
// a following "from" (for example "Select the current production line")
// passes clean, as do identifiers like "api_gateway_status" (no "api/"
// path segment) and "effective_from" ("from" is not word-bounded there).
var connectorShapePattern = regexp.MustCompile(`(?i)` +
	`[a-z][a-z0-9+.-]*://` +
	`|jdbc:` +
	`|(?:\b|\\[nrt])select\b[\s\S]{0,300}?(?:\b|\\[nrt])from\b` +
	`|select\*from` +
	`|(?:^|[^a-z0-9_])api/` +
	`|password\s*=` +
	`|\b(?:user id|uid|pwd)\s*=` +
	`|\b(?:data source|initial catalog|integrated security)\s*=` +
	`|\\\\[^\\]+\\\\` +
	`|authorization:\s*bearer`)

// NoConnectorLeak marshals v to JSON and reports ErrConnectorShapedContent
// if the result contains anything that looks like a connector endpoint,
// credential or raw query — see connectorShapePattern for the exact,
// exhaustive list of detected shapes (URL schemes, jdbc: URLs, SQL
// SELECT...FROM shapes, api/ path segments, connection-string credential
// keys such as password=/uid=/data source=, bearer authorization values,
// and backslash share paths). The Operating Map publishes enterprise
// cognition of WHERE and HOW to work — never a connector catalog — so
// every Entry (via Validate) and every resolved OperatingContext must pass
// this check.
func NoConnectorLeak(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	loc := connectorShapePattern.FindIndex(raw)
	if loc == nil {
		return nil
	}
	end := loc[1] + 24
	if end > len(raw) {
		end = len(raw)
	}
	return fmt.Errorf("%w: near %q", ErrConnectorShapedContent, raw[loc[0]:end])
}

var dottedIdentifierPattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

func validDottedIdentifier(v string) bool {
	return v != "" && utf8.RuneCountInString(v) <= 128 && dottedIdentifierPattern.MatchString(v)
}

func validAuthorityTier(v AuthorityTier) bool {
	switch v {
	case AuthoritySystemOfRecord, AuthorityDerived, AuthorityAdvisory:
		return true
	}
	return false
}

func validConflictResolution(v ConflictResolution) bool {
	switch v {
	case ResolvePreferAuthorityTier, ResolvePreferFreshest, ResolveEscalateHuman:
		return true
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

type needKey struct{ domain, needKind string }

// Validate reports whether e's caller-supplied content is well-formed:
// required fields are present and bounded, IntentKey and every
// BusinessCapability/MethodRef name are lowercase dotted identifiers,
// every DataNeed is well-formed and declared at most once, every
// AuthorityRule/FreshnessRule/ConflictRule/CorrelationRule references a
// DataNeed the entry actually declares, no two AuthorityRules (or
// FreshnessRules) for the same DataNeed disagree with each other WITHIN
// this entry, a DataNeed's own AuthorityTier/FreshnessRequirement agrees
// with its own AuthorityRule/FreshnessRule when one exists, and the entry
// contains no connector-shaped content anywhere (NoConnectorLeak).
//
// Validate does not check ID, Version or CreatedAt (the publishing service
// assigns those) and cannot check cross-entry publication conflicts or
// org_version staleness, which need the enterprise's other published
// entries and therefore live in the internal Service.
func (e Entry) Validate() error {
	if e.EnterpriseID == "" || utf8.RuneCountInString(e.EnterpriseID) > 128 {
		return fmt.Errorf("enterprise_id must contain 1..128 characters")
	}
	if e.OrgScope == "" || utf8.RuneCountInString(e.OrgScope) > 256 {
		return fmt.Errorf("org_scope must contain 1..256 characters")
	}
	if e.OrgVersion < 0 {
		return fmt.Errorf("org_version must be non-negative")
	}
	if !validDottedIdentifier(e.IntentKey) {
		return fmt.Errorf("intent_key must be a lowercase dotted identifier")
	}
	if len(e.IntentPhrases) == 0 {
		return fmt.Errorf("intent_phrases must contain at least one phrase")
	}
	seenPhrase := map[string]bool{}
	for _, p := range e.IntentPhrases {
		if p == "" || utf8.RuneCountInString(p) > 256 {
			return fmt.Errorf("intent_phrases entries must contain 1..256 characters")
		}
		if seenPhrase[p] {
			return fmt.Errorf("intent_phrases must not repeat %q", p)
		}
		seenPhrase[p] = true
	}
	if e.EffectiveFrom.IsZero() {
		return fmt.Errorf("effective_from is required")
	}
	if !e.EffectiveTo.IsZero() && !e.EffectiveTo.After(e.EffectiveFrom) {
		return fmt.Errorf("effective_to must be after effective_from when set")
	}
	if e.SourcePolicyRevision == "" || utf8.RuneCountInString(e.SourcePolicyRevision) > 128 {
		return fmt.Errorf("source_policy_revision must contain 1..128 characters")
	}
	if e.GovernanceReviewRef == "" || utf8.RuneCountInString(e.GovernanceReviewRef) > 128 {
		return fmt.Errorf("governance_review_ref must contain 1..128 characters")
	}
	if len(e.DataNeeds) == 0 {
		return fmt.Errorf("data_needs must contain at least one entry")
	}

	needs := map[needKey]DataNeed{}
	for _, n := range e.DataNeeds {
		if n.Domain == "" || utf8.RuneCountInString(n.Domain) > 64 {
			return fmt.Errorf("data_need domain must contain 1..64 characters")
		}
		if n.NeedKind == "" || utf8.RuneCountInString(n.NeedKind) > 128 {
			return fmt.Errorf("data_need need_kind must contain 1..128 characters")
		}
		if len(n.BusinessKeys) == 0 {
			return fmt.Errorf("data_need %s/%s must declare at least one business key", n.Domain, n.NeedKind)
		}
		for _, k := range n.BusinessKeys {
			if k == "" || utf8.RuneCountInString(k) > 128 {
				return fmt.Errorf("data_need %s/%s business keys must contain 1..128 characters", n.Domain, n.NeedKind)
			}
		}
		if !validAuthorityTier(n.AuthorityTier) {
			return fmt.Errorf("data_need %s/%s has an invalid authority_tier", n.Domain, n.NeedKind)
		}
		key := needKey{n.Domain, n.NeedKind}
		if _, dup := needs[key]; dup {
			return fmt.Errorf("data_need %s/%s is declared more than once", n.Domain, n.NeedKind)
		}
		needs[key] = n
	}

	authorityByNeed := map[needKey]AuthorityRule{}
	for _, r := range e.AuthorityRules {
		key := needKey{r.Domain, r.NeedKind}
		if _, ok := needs[key]; !ok {
			return fmt.Errorf("authority_rule %s/%s does not match a declared data_need", r.Domain, r.NeedKind)
		}
		if !validAuthorityTier(r.AuthorityTier) {
			return fmt.Errorf("authority_rule %s/%s has an invalid authority_tier", r.Domain, r.NeedKind)
		}
		if existing, dup := authorityByNeed[key]; dup && existing.AuthorityTier != r.AuthorityTier {
			return fmt.Errorf("authority_rule %s/%s conflicts with another authority_rule in the same entry (%q vs %q)", r.Domain, r.NeedKind, existing.AuthorityTier, r.AuthorityTier)
		}
		authorityByNeed[key] = r
	}

	freshnessByNeed := map[needKey]FreshnessRule{}
	for _, r := range e.FreshnessRules {
		key := needKey{r.Domain, r.NeedKind}
		if _, ok := needs[key]; !ok {
			return fmt.Errorf("freshness_rule %s/%s does not match a declared data_need", r.Domain, r.NeedKind)
		}
		if r.FreshnessRequirement == "" {
			return fmt.Errorf("freshness_rule %s/%s requires a freshness_requirement", r.Domain, r.NeedKind)
		}
		if existing, dup := freshnessByNeed[key]; dup && existing.FreshnessRequirement != r.FreshnessRequirement {
			return fmt.Errorf("freshness_rule %s/%s conflicts with another freshness_rule in the same entry (%q vs %q)", r.Domain, r.NeedKind, existing.FreshnessRequirement, r.FreshnessRequirement)
		}
		freshnessByNeed[key] = r
	}

	for key, need := range needs {
		if rule, ok := authorityByNeed[key]; ok && rule.AuthorityTier != need.AuthorityTier {
			return fmt.Errorf("data_need %s/%s authority_tier %q disagrees with its own authority_rule %q", key.domain, key.needKind, need.AuthorityTier, rule.AuthorityTier)
		}
		if rule, ok := freshnessByNeed[key]; ok && need.FreshnessRequirement != "" && rule.FreshnessRequirement != need.FreshnessRequirement {
			return fmt.Errorf("data_need %s/%s freshness_requirement %q disagrees with its own freshness_rule %q", key.domain, key.needKind, need.FreshnessRequirement, rule.FreshnessRequirement)
		}
	}

	for _, r := range e.ConflictRules {
		key := needKey{r.Domain, r.NeedKind}
		if _, ok := needs[key]; !ok {
			return fmt.Errorf("conflict_rule %s/%s does not match a declared data_need", r.Domain, r.NeedKind)
		}
		if !validConflictResolution(r.Resolution) {
			return fmt.Errorf("conflict_rule %s/%s has an invalid resolution", r.Domain, r.NeedKind)
		}
	}

	for _, r := range e.CorrelationRules {
		if r.BusinessKey == "" {
			return fmt.Errorf("correlation_rule requires a business_key")
		}
		if len(r.NeedKinds) < 2 {
			return fmt.Errorf("correlation_rule %s must correlate at least two need_kinds", r.BusinessKey)
		}
		for _, nk := range r.NeedKinds {
			found := false
			for key, need := range needs {
				if key.needKind != nk {
					continue
				}
				found = true
				if !containsString(need.BusinessKeys, r.BusinessKey) {
					return fmt.Errorf("correlation_rule %s references need_kind %s, which does not declare that business key", r.BusinessKey, nk)
				}
			}
			if !found {
				return fmt.Errorf("correlation_rule %s references unknown need_kind %s", r.BusinessKey, nk)
			}
		}
	}

	for _, m := range e.MethodRefs {
		if !validDottedIdentifier(m.BusinessCapability) {
			return fmt.Errorf("method_ref business_capability must be a lowercase dotted identifier")
		}
		if m.Kind == "" {
			return fmt.Errorf("method_ref requires a kind")
		}
	}
	for _, c := range e.BusinessCapabilities {
		if !validDottedIdentifier(c.Name) {
			return fmt.Errorf("business_capability name must be a lowercase dotted identifier")
		}
	}
	for _, m := range e.MemoryRefs {
		if m.Handle == "" || m.ContentHash == "" {
			return fmt.Errorf("memory_ref requires a handle and content_hash")
		}
	}

	return NoConnectorLeak(e)
}
