package retrieval

// Task 18A Part A — knowledge-authority classification.
//
// The per-enterprise knowledge index mixes governed, evidence-grounded facts
// (0J-published method_outline, verified-outcome and timeline facts, grounded
// document summaries) with the legacy Dream summarizer's ungrounded LLM
// narrative digest. Only the grounded facts may be surfaced, cited or published
// as governed enterprise knowledge; the ungrounded digest must be demoted so it
// can never become governed knowledge/method/Operating-Map/risk/assessment.
//
// Classification is by source_type at the retrieval/answer layer. This means
// EXISTING indexed documents are classified correctly with no re-index and the
// OpenSearch mapping is unchanged (documents carry only source_type). The
// Authoritative stamp on IndexDocument (indexer.go) is additional defense in
// depth for newly indexed documents; the source_type classification remains the
// authority for anything already in the index.

// nonAuthoritativeSourceTypes enumerates knowledge-index source types whose
// documents are ungrounded narrative digests — text NOT grounded in a verified
// Outcome, signed receipt, authorized artifact or governed publication. The
// legacy Dream summarizer (internal/dream) is the only such producer today: it
// writes dream_summary layers into the knowledge index AND stamps the
// dream-derived timeline node it emits with source_type "dream_summary"
// (see internal/dream/runner.go). Adding any new ungrounded index source must
// extend this set and pass the authoritative-boundary review.
var nonAuthoritativeSourceTypes = map[string]bool{
	"dream_summary": true,
}

// SourceTypeAuthoritative reports whether documents of the given knowledge-index
// source type may be cited or published as governed enterprise knowledge.
// Grounded facts — verified Outcomes, timeline facts (e.g. work_brief,
// risk_event), 0J-published method_outline, grounded document_summary — are
// authoritative; the ungrounded legacy dream_summary digest is not.
func SourceTypeAuthoritative(sourceType string) bool {
	return !nonAuthoritativeSourceTypes[sourceType]
}

// GovernedKnowledge partitions retrieval results into those that may ground a
// governed-knowledge answer/citation (authoritative) and the demoted,
// non-authoritative digests (e.g. the legacy dream_summary) that must never be
// presented or published as governed knowledge. The demoted set is returned
// separately so a caller may still surface it — searchable, but clearly
// labelled non-authoritative — never as governed knowledge.
func GovernedKnowledge(results []Result) (authoritative, nonAuthoritative []Result) {
	for _, r := range results {
		if r.Authoritative {
			authoritative = append(authoritative, r)
		} else {
			nonAuthoritative = append(nonAuthoritative, r)
		}
	}
	return authoritative, nonAuthoritative
}
