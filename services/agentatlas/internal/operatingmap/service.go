// Package operatingmap implements the enterprise Operating Map: versioned
// publication and resolution of WHERE and HOW to work for an employee
// intent, scoped to (enterprise, org_scope) and bound to enterprise
// governance (effective interval, source policy revision, governance
// review). ResolveIntent returns business semantics only — a DataNeed,
// MethodRef or BusinessCapability never carries a connector endpoint,
// credential or raw request body; see model.NoConnectorLeak, which every
// Publish and every ResolveIntent result passes through.
package operatingmap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
)

var (
	// ErrInvalidRequest is returned by ResolveIntent when enterpriseID,
	// orgScope or intent is empty.
	ErrInvalidRequest = errors.New("operatingmap: enterprise_id, org_scope and intent are required")

	// ErrIntentNotResolved is returned when no published, effective,
	// non-stale entry in (enterpriseID, orgScope) registers intent as one
	// of its intent_phrases. It is also what a cross-tenant or cross-scope
	// resolution attempt sees: a wrong-tenant or wrong-scope read must look
	// exactly like "nothing published here", never leak that something is
	// published elsewhere.
	ErrIntentNotResolved = errors.New("operatingmap: intent did not resolve to a published, effective operating map entry")

	// ErrRuleConflict is returned by Publish when candidate's authority or
	// freshness rules disagree with a DIFFERENT already-published entry's
	// rules for the same (domain, need_kind) over an overlapping effective
	// interval in the same (enterprise, org_scope). Republishing a NEW
	// version of the SAME intent_key is supersession, not conflict, and is
	// always allowed.
	ErrRuleConflict = errors.New("operatingmap: conflicting authority or freshness rule for the same data need in the same scope and interval")
)

// RuleConflictError carries the detail behind ErrRuleConflict.
type RuleConflictError struct {
	Domain, NeedKind     string
	RuleType             string // "authority_tier" | "freshness_requirement"
	CandidateValue       string
	ConflictingEntryID   string
	ConflictingIntentKey string
	ConflictingValue     string
}

func (e *RuleConflictError) Error() string {
	return fmt.Sprintf("%s: domain=%s need_kind=%s rule=%s candidate=%q conflicts with entry %s (intent_key=%s) value=%q",
		ErrRuleConflict, e.Domain, e.NeedKind, e.RuleType, e.CandidateValue, e.ConflictingEntryID, e.ConflictingIntentKey, e.ConflictingValue)
}

func (e *RuleConflictError) Unwrap() error { return ErrRuleConflict }

// Store persists Operating Map entries. Every method is tenant-scoped by
// (enterpriseID, orgScope) exact match — never a prefix or hierarchy walk —
// so a wrong-tenant or wrong-scope caller sees an empty result, not another
// tenant's data.
type Store interface {
	// Publish assigns candidate the next version for its
	// (enterprise_id, org_scope, intent_key) triple, invokes check with
	// every OTHER entry in (candidate.EnterpriseID, candidate.OrgScope) —
	// across all intent keys — whose half-open effective interval
	// [EffectiveFrom, EffectiveTo) intersects candidate's own (EffectiveTo
	// zero means +infinity), and — only if check returns nil — persists
	// candidate as a new immutable row and returns it with ID, Version and
	// CreatedAt populated. check receives ALL overlapping entries,
	// including superseded older versions of the same intent keys; the
	// shared detectRuleConflicts performs the supersession reduction
	// itself, so implementations need no version filtering of their own.
	// If check returns an error, Publish returns that error unchanged and
	// persists nothing. Implementations must make version assignment, the
	// check and the insert atomic with respect to concurrent Publish calls
	// for the same (enterprise_id, org_scope).
	Publish(ctx context.Context, candidate model.Entry, check func(overlapping []model.Entry) error) (model.Entry, error)

	// ActiveEntries returns every entry for (enterpriseID, orgScope) whose
	// half-open effective interval [EffectiveFrom, EffectiveTo) covers
	// asOf (EffectiveTo zero means +infinity), across all intent keys.
	// Returned entries must be fresh, per-call values: ResolveIntent
	// aliases their slices directly into the OperatingContext it returns
	// (see its doc comment), so an implementation that serves shared or
	// cached Entry values must deep-copy them first.
	ActiveEntries(ctx context.Context, enterpriseID, orgScope string, asOf time.Time) ([]model.Entry, error)
}

// Service is the Operating Map's business logic: publication governance
// (conflict detection across entries) and intent resolution (staleness and
// expiry filtering, phrase matching, content aggregation). Persistence is
// delegated to Store.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService builds a Service. now defaults to time.Now.
func NewService(store Store, now func() time.Time) (*Service, error) {
	if store == nil {
		return nil, errors.New("operatingmap: service requires a store")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}, nil
}

// Publish validates candidate, checks it for authority/freshness conflicts
// against every other currently-published entry in the same
// (enterprise, org_scope) whose effective interval overlaps candidate's,
// and — if none is found — persists it as a new immutable version. ID,
// Version and CreatedAt on candidate are ignored; the returned Entry
// carries the values the Store assigned.
func (s *Service) Publish(ctx context.Context, candidate model.Entry) (model.Entry, error) {
	candidate.ID = ""
	candidate.Version = 0
	candidate.CreatedAt = time.Time{}
	if err := candidate.Validate(); err != nil {
		return model.Entry{}, err
	}
	candidate.CreatedAt = s.now().UTC()
	check := func(overlapping []model.Entry) error {
		return detectRuleConflicts(candidate, overlapping)
	}
	return s.store.Publish(ctx, candidate, check)
}

// ResolveIntent looks up published Operating Map entries by enterprise, org
// scope and intent semantics, and returns the business semantics they
// bundle: matched MethodRefs, DataNeeds, BusinessCapabilities and
// applicable rules. It never returns credentials, connector URLs or
// provider-specific request bodies (see model.NoConnectorLeak).
//
// Resolution excludes: entries whose effective interval does not cover now
// (expired or not-yet-effective), and — among entries that ARE effective —
// any entry whose org_version is lower than the highest org_version bound
// to any effective entry in the same (enterpriseID, orgScope). The latter
// is this Service's deliberate, fail-closed definition of "current org
// version": the Operating Map has no other signal that the org changed
// shape, so the most recent org_version any publisher has asserted for the
// scope is treated as authoritative, and older entries are treated as
// unverified against it until they are republished (see notes.md for the
// full rationale). When multiple entries (necessarily under different
// intent keys) match the same intent phrase, their content is aggregated
// into one OperatingContext, sorted by intent_key for determinism.
//
// Aliasing note: the returned OperatingContext aggregates — and therefore
// aliases — the slices of the entries the Store returned; callers must
// treat it as read-only. This is safe with both current Store
// implementations because each builds fresh values per call
// (PostgresStore's scanEntries unmarshals a fresh entryContent per row;
// the in-memory test Store only ever appends), but a future caching Store
// MUST deep-copy entries before returning them, or a context handed to
// one caller could be mutated through another.
func (s *Service) ResolveIntent(ctx context.Context, enterpriseID, orgScope, intent string) (model.OperatingContext, error) {
	if enterpriseID == "" || orgScope == "" || intent == "" {
		return model.OperatingContext{}, ErrInvalidRequest
	}
	asOf := s.now().UTC()
	active, err := s.store.ActiveEntries(ctx, enterpriseID, orgScope, asOf)
	if err != nil {
		return model.OperatingContext{}, err
	}

	latest := map[string]model.Entry{}
	for _, e := range active {
		if cur, ok := latest[e.IntentKey]; !ok || e.Version > cur.Version {
			latest[e.IntentKey] = e
		}
	}
	var currentOrgVersion int64
	for _, e := range latest {
		if e.OrgVersion > currentOrgVersion {
			currentOrgVersion = e.OrgVersion
		}
	}

	var matched []model.Entry
	for _, e := range latest {
		if e.OrgVersion != currentOrgVersion || !containsExact(e.IntentPhrases, intent) {
			continue
		}
		matched = append(matched, e)
	}
	if len(matched) == 0 {
		return model.OperatingContext{}, ErrIntentNotResolved
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].IntentKey < matched[j].IntentKey })

	out := model.OperatingContext{
		Intent:              intent,
		OrganizationContext: model.OrganizationContext{EnterpriseID: enterpriseID, OrgScope: orgScope, OrgVersion: currentOrgVersion},
	}
	for _, e := range matched {
		out.DataNeeds = append(out.DataNeeds, e.DataNeeds...)
		out.MethodRefs = append(out.MethodRefs, e.MethodRefs...)
		out.BusinessCapabilities = append(out.BusinessCapabilities, e.BusinessCapabilities...)
		out.CorrelationRules = append(out.CorrelationRules, e.CorrelationRules...)
		out.AuthorityRules = append(out.AuthorityRules, e.AuthorityRules...)
		out.FreshnessRules = append(out.FreshnessRules, e.FreshnessRules...)
		out.ConflictRules = append(out.ConflictRules, e.ConflictRules...)
		out.MemoryRefs = append(out.MemoryRefs, e.MemoryRefs...)
	}
	if err := model.NoConnectorLeak(out); err != nil {
		return model.OperatingContext{}, err
	}
	return out, nil
}

func containsExact(phrases []string, intent string) bool {
	for _, p := range phrases {
		if p == intent {
			return true
		}
	}
	return false
}

// ruleKey identifies the (domain, need_kind) an AuthorityRule or
// FreshnessRule governs.
type ruleKey struct{ domain, needKind string }

// detectRuleConflicts reports whether candidate's authority or freshness
// rules disagree with the rules already published under a DIFFERENT
// intent_key among overlapping — the entries Store.Publish found whose
// effective interval intersects candidate's, in the same
// (enterprise_id, org_scope). Two disciplines keep the gate sound and
// deterministic:
//
//  1. Supersession reduction: overlapping is first deduplicated to the
//     LATEST version per intent key — the same max-version reduction
//     ResolveIntent applies — because publishing a new version supersedes
//     the older versions of that intent key everywhere, even where an
//     older open-ended version's effective interval still lingers in the
//     table (published rows are immutable, so a superseded open-ended v1
//     row stays "effective" forever; only v2 is ever surfaced to
//     resolution, so only v2 may block or excuse a new publication).
//     Without this reduction the gate's verdict could depend on store row
//     order whenever a superseded version disagrees with its successor.
//     Entries sharing candidate's own intent_key are candidate's own
//     lineage (supersession, a new version legitimately changing the
//     rule) and are ignored entirely.
//  2. Deterministic comparison order: the deduplicated neighbors are
//     walked in sorted intent-key order, so which conflicting entry gets
//     NAMED in the returned error never depends on map or row order.
//
// This is the shared, pure algorithm behind the mandated "conflicting
// authority/freshness rules fail publication" test — used unchanged by
// both PostgresStore and the in-memory test Store, so their conflict
// semantics cannot drift apart.
func detectRuleConflicts(candidate model.Entry, overlapping []model.Entry) error {
	latest := map[string]model.Entry{}
	for _, e := range overlapping {
		if e.IntentKey == candidate.IntentKey {
			continue
		}
		if cur, ok := latest[e.IntentKey]; !ok || e.Version > cur.Version {
			latest[e.IntentKey] = e
		}
	}
	neighborKeys := make([]string, 0, len(latest))
	for k := range latest {
		neighborKeys = append(neighborKeys, k)
	}
	sort.Strings(neighborKeys)

	authority := map[ruleKey]struct{ tier, entryID, intentKey string }{}
	freshness := map[ruleKey]struct{ window, entryID, intentKey string }{}
	for _, k := range neighborKeys {
		e := latest[k]
		for _, r := range e.AuthorityRules {
			authority[ruleKey{r.Domain, r.NeedKind}] = struct{ tier, entryID, intentKey string }{string(r.AuthorityTier), e.ID, e.IntentKey}
		}
		for _, r := range e.FreshnessRules {
			freshness[ruleKey{r.Domain, r.NeedKind}] = struct{ window, entryID, intentKey string }{r.FreshnessRequirement, e.ID, e.IntentKey}
		}
	}
	for _, r := range candidate.AuthorityRules {
		if existing, ok := authority[ruleKey{r.Domain, r.NeedKind}]; ok && existing.tier != string(r.AuthorityTier) {
			return &RuleConflictError{Domain: r.Domain, NeedKind: r.NeedKind, RuleType: "authority_tier", CandidateValue: string(r.AuthorityTier), ConflictingEntryID: existing.entryID, ConflictingIntentKey: existing.intentKey, ConflictingValue: existing.tier}
		}
	}
	for _, r := range candidate.FreshnessRules {
		if existing, ok := freshness[ruleKey{r.Domain, r.NeedKind}]; ok && existing.window != r.FreshnessRequirement {
			return &RuleConflictError{Domain: r.Domain, NeedKind: r.NeedKind, RuleType: "freshness_requirement", CandidateValue: r.FreshnessRequirement, ConflictingEntryID: existing.entryID, ConflictingIntentKey: existing.intentKey, ConflictingValue: existing.window}
		}
	}
	return nil
}
