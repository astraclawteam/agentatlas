package retrieval

import (
	"sort"
	"time"
)

// Query is the retrieval request scoped to one enterprise.
type Query struct {
	EnterpriseID string
	Text         string
	SpaceIDs     []string
	OrgScopes    []string
	SourceTypes  []string
	MaxRiskLevel string // low | medium | high; docs at or below pass
	From, To     *time.Time
	TopK         int
}

func (q Query) topK() int {
	if q.TopK <= 0 {
		return 10
	}
	return q.TopK
}

// allowedRiskLevels expands "docs at or below MaxRiskLevel".
func allowedRiskLevels(max string) []string {
	switch max {
	case "low":
		return []string{"low", ""}
	case "", "medium":
		return []string{"low", "medium", ""}
	default:
		return []string{"low", "medium", "high", ""}
	}
}

func buildFilters(q Query) []map[string]any {
	// enterprise_id filter is defense in depth on top of per-enterprise indexes
	filters := []map[string]any{
		{"term": map[string]any{"enterprise_id": q.EnterpriseID}},
	}
	if len(q.SpaceIDs) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"space_id": q.SpaceIDs}})
	}
	if len(q.OrgScopes) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"org_scope": q.OrgScopes}})
	}
	if len(q.SourceTypes) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"source_type": q.SourceTypes}})
	}
	levels := allowedRiskLevels(q.MaxRiskLevel)
	filters = append(filters, map[string]any{
		"bool": map[string]any{
			"should": []map[string]any{
				{"terms": map[string]any{"risk_level": levels}},
				{"bool": map[string]any{"must_not": []map[string]any{{"exists": map[string]any{"field": "risk_level"}}}}},
			},
		},
	})
	if q.From != nil || q.To != nil {
		rangeQ := map[string]any{}
		if q.From != nil {
			rangeQ["gte"] = q.From.Format(time.RFC3339)
		}
		if q.To != nil {
			rangeQ["lte"] = q.To.Format(time.RFC3339)
		}
		filters = append(filters, map[string]any{"range": map[string]any{"node_time": rangeQ}})
	}
	return filters
}

// BuildKeywordQuery: smartcn-analyzed multi_match over summary and snippet.
func BuildKeywordQuery(q Query) map[string]any {
	return map[string]any{
		"size": q.topK(),
		"query": map[string]any{
			"bool": map[string]any{
				"must": []map[string]any{{
					"multi_match": map[string]any{
						"query":  q.Text,
						"fields": []string{"summary_text^2", "sanitized_snippet"},
					},
				}},
				"filter": buildFilters(q),
			},
		},
	}
}

// BuildVectorQuery: kNN recall under the same structured filters.
func BuildVectorQuery(q Query, embedding []float32) map[string]any {
	return map[string]any{
		"size": q.topK(),
		"query": map[string]any{
			"bool": map[string]any{
				"must": []map[string]any{{
					"knn": map[string]any{
						"embedding": map[string]any{
							"vector": embedding,
							"k":      q.topK(),
						},
					},
				}},
				"filter": buildFilters(q),
			},
		},
	}
}

// RRFMerge fuses keyword and vector rankings with reciprocal rank fusion
// (k=60), deduplicating by document id. Deterministic: ties break by id.
func RRFMerge(keyword, vector []Hit, topK int) []Hit {
	const k = 60
	type fused struct {
		hit   Hit
		score float64
	}
	byID := map[string]*fused{}
	accumulate := func(hits []Hit) {
		for rank, h := range hits {
			f, ok := byID[h.ID]
			if !ok {
				f = &fused{hit: h}
				byID[h.ID] = f
			}
			f.score += 1.0 / float64(k+rank+1)
		}
	}
	accumulate(keyword)
	accumulate(vector)

	out := make([]fused, 0, len(byID))
	for _, f := range byID {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].hit.ID < out[j].hit.ID
	})
	if len(out) > topK {
		out = out[:topK]
	}
	hits := make([]Hit, len(out))
	for i, f := range out {
		hits[i] = f.hit
		hits[i].Score = f.score
	}
	return hits
}
