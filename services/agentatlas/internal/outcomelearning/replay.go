package outcomelearning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// replay.go re-derives a candidate's claimed effect against the EXACT immutable
// historical Outcome versions (Tasks 0G/0H are append-only + immutable) and
// runs a bounded shadow comparison against the status quo. Both are pure and
// deterministic: identical inputs yield an identical result and digest, so a
// candidate can be replayed twice with byte-identical outcomes and a replay
// mismatch (the read-model claim disagreeing with authoritative history) or a
// shadow regression (the proposed method underperforming the baseline) is a
// stable, reproducible verdict.

// authView is the immutable authoritative snapshot the pipeline replays and
// shadows against. It is read once from the OutcomeVersions store at the exact
// bound revisions; nothing here writes.
type authView struct {
	// atRevision maps an outcome business key -> the exact bound-revision
	// Outcome the graph referenced (the version being replayed).
	atRevision map[string]outcomemodel.Outcome
	// latestRevision maps an outcome business key -> its current head revision.
	latestRevision map[string]uint64
	// present marks whether the exact bound revision was found.
	present map[string]bool
}

// --- replay ----------------------------------------------------------------

// outcomeFact is one deterministic, structural fact about a replayed outcome.
type outcomeFact struct {
	Key                string `json:"key"`
	Revision           uint64 `json:"revision"`
	SubjectContributed bool   `json:"subject_contributed"`
	Status             string `json:"status"`
	Current            bool   `json:"current"` // bound revision is still the head and not superseded
}

// Effect is a deterministic structural fingerprint of a replayed source set.
type Effect struct {
	Subject string        `json:"subject"`
	Facts   []outcomeFact `json:"facts"`
}

func (e Effect) digest() string {
	sort.Slice(e.Facts, func(i, j int) bool {
		if e.Facts[i].Key != e.Facts[j].Key {
			return e.Facts[i].Key < e.Facts[j].Key
		}
		return e.Facts[i].Revision < e.Facts[j].Revision
	})
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ReplayResult is the deterministic result of replaying a candidate's claim
// against immutable history.
type ReplayResult struct {
	Matched  bool   `json:"matched"`
	Claimed  int    `json:"claimed"`
	Observed int    `json:"observed"`
	Digest   string `json:"digest"`
	Mismatch string `json:"mismatch,omitempty"`
}

// computeReplay verifies the graph's structural claim ("subject is bound to
// these outcomes, which are current") against authoritative immutable history.
// member reports whether an authoritative Outcome actually binds the subject
// (a contribution for a method candidate, a blocker for a recurring-exception
// candidate). A mismatch — the subject not actually bound on a claimed outcome,
// a bound revision absent, or a currentness disagreement (the bound revision has
// since been superseded) — yields Matched=false and quarantines the candidate.
func computeReplay(subject string, member func(outcomemodel.Outcome) bool, claimed []SourceRef, graphRevoked map[string]bool, view authView) ReplayResult {
	e := Effect{Subject: subject}
	matched := true
	mismatch := ""
	observed := 0
	for _, ref := range claimed {
		o, ok := view.atRevision[ref.BusinessID]
		fact := outcomeFact{Key: ref.BusinessID, Revision: ref.Revision}
		if !ok || !view.present[ref.BusinessID] {
			matched = false
			if mismatch == "" {
				mismatch = "claimed source outcome " + ref.BusinessID + " is not present at the bound revision"
			}
			e.Facts = append(e.Facts, fact)
			continue
		}
		fact.Status = string(o.Claim.Status)
		fact.SubjectContributed = member(o)
		// Current iff the bound revision is still the head and the graph did not
		// flag it revoked/superseded.
		head := view.latestRevision[ref.BusinessID]
		fact.Current = head == ref.Revision && !graphRevoked[ref.BusinessID]
		if !fact.SubjectContributed {
			matched = false
			if mismatch == "" {
				mismatch = "authoritative outcome " + ref.BusinessID + " does not bind the claimed subject"
			}
		}
		if !fact.Current {
			matched = false
			if mismatch == "" {
				mismatch = "authoritative outcome " + ref.BusinessID + " bound revision is no longer current"
			}
		}
		if fact.SubjectContributed {
			observed++
		}
		e.Facts = append(e.Facts, fact)
	}
	return ReplayResult{Matched: matched, Claimed: len(claimed), Observed: observed, Digest: e.digest(), Mismatch: mismatch}
}

// contributes reports whether o credits contributor subject.
func contributes(o outcomemodel.Outcome, subject string) bool {
	for _, c := range o.Contributions {
		if c.ContributorID == subject {
			return true
		}
	}
	return false
}

// blockedBy reports whether o names blocker handle in its claim blockers.
func blockedBy(o outcomemodel.Outcome, blocker string) bool {
	for _, b := range o.Claim.Blockers {
		if b.Handle == blocker {
			return true
		}
	}
	return false
}

// --- shadow ----------------------------------------------------------------

// ShadowResult is the deterministic result of the bounded shadow comparison:
// the proposed method/rule's satisfied-rate against the status-quo baseline over
// a bounded historical window.
type ShadowResult struct {
	Applicable bool    `json:"applicable"`
	Baseline   float64 `json:"baseline"`
	Candidate  float64 `json:"candidate"`
	Regression bool    `json:"regression"`
	Samples    int     `json:"samples"`
	Bounded    bool    `json:"bounded"`
	Digest     string  `json:"digest"`
}

func (r ShadowResult) recompute() ShadowResult {
	raw, _ := json.Marshal(struct {
		Applicable bool    `json:"applicable"`
		Baseline   float64 `json:"baseline"`
		Candidate  float64 `json:"candidate"`
		Regression bool    `json:"regression"`
		Samples    int     `json:"samples"`
		Bounded    bool    `json:"bounded"`
	}{r.Applicable, r.Baseline, r.Candidate, r.Regression, r.Samples, r.Bounded})
	sum := sha256.Sum256(raw)
	r.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return r
}

// computeShadow compares the subject's satisfied-rate against the baseline
// satisfied-rate over the goal's historical outcomes, bounded by budget. A
// candidate that does not strictly beat the baseline is a regression and is
// quarantined. Only terminal (satisfied/unsatisfied) outcomes count toward a
// rate; the window is sorted deterministically and capped at budget.
func computeShadow(subject string, subjectSources, windowSources []SourceRef, view authView, budget int) ShadowResult {
	window := sortedKeys(windowSources)
	bounded := false
	if budget > 0 && len(window) > budget {
		window = window[:budget]
		bounded = true
	}
	baseSat, baseTotal := rate(window, "", view)
	subjSat, subjTotal := rate(sortedKeys(subjectSources), subject, view)

	baseline := ratio(baseSat, baseTotal)
	candidate := ratio(subjSat, subjTotal)
	// A regression is any candidate rate that does not STRICTLY improve on the
	// status quo (fail-closed: "no measured improvement" is not adoptable).
	regression := subjTotal == 0 || candidate <= baseline
	res := ShadowResult{
		Applicable: true, Baseline: baseline, Candidate: candidate,
		Regression: regression, Samples: len(window), Bounded: bounded,
	}
	return res.recompute()
}

// notApplicableShadow is used for kinds without a method/rule to shadow (e.g.
// recurring blocker patterns): there is no status-quo rate to regress against.
func notApplicableShadow() ShadowResult {
	return ShadowResult{Applicable: false}.recompute()
}

func sortedKeys(refs []SourceRef) []string {
	keys := make([]string, 0, len(refs))
	for _, r := range refs {
		keys = append(keys, r.BusinessID)
	}
	sort.Strings(keys)
	return keys
}

// rate counts satisfied vs terminal outcomes over keys. When subject != "",
// only outcomes the subject contributed to are counted.
func rate(keys []string, subject string, view authView) (sat, total int) {
	for _, k := range keys {
		o, ok := view.atRevision[k]
		if !ok {
			continue
		}
		if subject != "" && !contributes(o, subject) {
			continue
		}
		switch o.Claim.Status {
		case outcomemodel.OutcomeSatisfied:
			sat++
			total++
		case outcomemodel.OutcomeUnsatisfied:
			total++
		}
	}
	return sat, total
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
