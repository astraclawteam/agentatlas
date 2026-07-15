package retrieval

import (
	"context"
	"testing"
)

// Task 18A Part A — the authoritative-vs-non-authoritative knowledge boundary.
//
// The legacy Dream summarizer (internal/dream) auto-indexes an ungrounded LLM
// narrative digest into the SAME per-enterprise knowledge index as governed,
// evidence-grounded facts (0J method_outline, verified-outcome/timeline facts)
// with no authority distinction. These tests pin the guard that classifies a
// dream_summary document as NON-AUTHORITATIVE and forbids the answer/knowledge
// surface from ever presenting or publishing it as governed knowledge, while a
// governed method_outline / verified-outcome / timeline document stays
// AUTHORITATIVE. Classification is by source_type at the retrieval/answer layer
// so existing indexed documents need no re-index and no OpenSearch mapping
// change is required.

func TestAuthoritativeBoundaryClassifiesSourceTypes(t *testing.T) {
	cases := []struct {
		sourceType string
		want       bool
	}{
		// NON-AUTHORITATIVE: the ungrounded legacy Dream digest. internal/dream
		// stamps both its summary layers AND the timeline node it emits with
		// source_type "dream_summary".
		{"dream_summary", false},

		// AUTHORITATIVE: governed / evidence-grounded knowledge sources.
		{"method_outline", true},   // 0J-published method outline
		{"outcome", true},          // verified Outcome fact
		{"timeline_node", true},    // timeline fact
		{"work_brief", true},       // grounded employee work-brief timeline fact
		{"risk_event", true},       // grounded risk timeline fact
		{"document_summary", true}, // grounded document summary (evidence-pointed)

		// Unknown/other grounded source types stay authoritative; only the
		// enumerated ungrounded digests are demoted.
		{"sop", true},
	}
	for _, tc := range cases {
		if got := SourceTypeAuthoritative(tc.sourceType); got != tc.want {
			t.Fatalf("SourceTypeAuthoritative(%q) = %v, want %v", tc.sourceType, got, tc.want)
		}
	}
}

// mixedIndex seeds one ungrounded dream_summary alongside governed documents.
func mixedIndex() *fakeSearch {
	return &fakeSearch{docs: map[string]IndexDocument{
		"dream1":  {EnterpriseID: "ent_1", SourceType: "dream_summary", SummaryText: "叙述摘要：本周整体顺利", SanitizedSnippet: "叙述摘要：本周整体顺利"},
		"method1": {EnterpriseID: "ent_1", SourceType: "method_outline", SummaryText: "治理方法：两步校验", SanitizedSnippet: "治理方法：两步校验"},
		"brief1":  {EnterpriseID: "ent_1", SourceType: "work_brief", SummaryText: "完成分拣规则联调", SanitizedSnippet: "完成分拣规则联调", EvidencePointerID: "ev_1"},
	}}
}

func TestAuthoritativeBoundaryExecuteMarksResults(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakePlanStore{}, mixedIndex(), nil, nil)
	q := Query{EnterpriseID: "ent_1", Text: "本周工作", TopK: 10}
	results, err := svc.Execute(ctx, "rp_test", q)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	byID := map[string]Result{}
	for _, r := range results {
		byID[r.DocID] = r
	}
	if len(byID) != 3 {
		t.Fatalf("want 3 results, got %d", len(byID))
	}
	if byID["dream1"].Authoritative {
		t.Fatalf("dream_summary result must be marked NON-authoritative")
	}
	if !byID["method1"].Authoritative {
		t.Fatalf("method_outline result must be marked authoritative")
	}
	if !byID["brief1"].Authoritative {
		t.Fatalf("timeline work_brief result must be marked authoritative")
	}
}

func TestAuthoritativeBoundaryGovernedKnowledgeNeverPublishesDreamSummary(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakePlanStore{}, mixedIndex(), nil, nil)
	results, err := svc.Execute(ctx, "rp_test", Query{EnterpriseID: "ent_1", Text: "本周工作", TopK: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	governed, nonAuthoritative := GovernedKnowledge(results)

	// A dream_summary must NEVER appear in the governed-knowledge citation set.
	for _, r := range governed {
		if r.SourceType == "dream_summary" || !r.Authoritative {
			t.Fatalf("governed-knowledge set leaked a non-authoritative doc: %+v", r)
		}
	}
	// It is preserved (searchable) but demoted into the non-authoritative set.
	foundDemoted := false
	for _, r := range nonAuthoritative {
		if r.DocID == "dream1" {
			foundDemoted = true
		}
		if r.Authoritative {
			t.Fatalf("non-authoritative set contains an authoritative doc: %+v", r)
		}
	}
	if !foundDemoted {
		t.Fatalf("dream_summary digest must remain searchable in the demoted non-authoritative set")
	}
	// The governed set is exactly the two grounded documents.
	if len(governed) != 2 {
		t.Fatalf("want 2 governed docs, got %d: %+v", len(governed), governed)
	}

	// A result set that is ONLY dream_summary can publish nothing as governed.
	onlyDream := []Result{{DocID: "d", SourceType: "dream_summary", Authoritative: false}}
	g, _ := GovernedKnowledge(onlyDream)
	if len(g) != 0 {
		t.Fatalf("an all-dream_summary result set must yield an EMPTY governed set, got %d", len(g))
	}
}
