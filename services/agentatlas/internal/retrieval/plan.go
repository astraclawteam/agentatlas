package retrieval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// PlanStore persists retrieval plans and results (db/generated satisfies it).
type PlanStore interface {
	CreateRetrievalPlan(ctx context.Context, arg db.CreateRetrievalPlanParams) (db.RetrievalPlan, error)
	InsertRetrievalPlanStep(ctx context.Context, arg db.InsertRetrievalPlanStepParams) error
	InsertRetrievalResult(ctx context.Context, arg db.InsertRetrievalResultParams) error
}

// Service executes hybrid retrieval: keyword + vector under structured
// filters, RRF fusion, optional rerank — returning evidence pointers and
// sanitized snippets only.
type Service struct {
	store    PlanStore
	client   SearchClient
	embedder Embedder // nil => keyword-only deployment (no embedding route configured)
	reranker Reranker // nil => fusion order stands
	metrics  *observability.Metrics
}

func NewService(store PlanStore, client SearchClient, embedder Embedder, reranker Reranker) *Service {
	return &Service{store: store, client: client, embedder: embedder, reranker: reranker}
}

type Result struct {
	DocID             string  `json:"doc_id"`
	SpaceID           string  `json:"space_id"`
	SourceType        string  `json:"source_type"`
	Snippet           string  `json:"snippet"`
	EvidencePointerID string  `json:"evidence_pointer_id"`
	Score             float64 `json:"score"`
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// CreatePlan persists the plan and its steps, mirroring what Execute will do.
func (s *Service) CreatePlan(ctx context.Context, q Query) (string, error) {
	filters, err := json.Marshal(map[string]any{
		"source_types": q.SourceTypes, "max_risk_level": q.MaxRiskLevel,
	})
	if err != nil {
		return "", err
	}
	plan, err := s.store.CreateRetrievalPlan(ctx, db.CreateRetrievalPlanParams{
		ID: newID("rp"), EnterpriseID: q.EnterpriseID,
		QueryHash: hashText(q.Text), SanitizedQuery: truncateRunes(q.Text, 1000),
		SpaceIds: orEmpty(q.SpaceIDs), OrgScopes: orEmpty(q.OrgScopes), Filters: filters,
	})
	if err != nil {
		return "", fmt.Errorf("create plan: %w", err)
	}
	stepNo := int32(1)
	addStep := func(kind string, params map[string]any) error {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if err := s.store.InsertRetrievalPlanStep(ctx, db.InsertRetrievalPlanStepParams{
			PlanID: plan.ID, StepNo: stepNo, Kind: kind, Params: raw,
		}); err != nil {
			return err
		}
		stepNo++
		return nil
	}
	if err := addStep("keyword", map[string]any{"top_k": q.topK()}); err != nil {
		return "", err
	}
	if s.embedder != nil {
		if err := addStep("vector", map[string]any{"top_k": q.topK(), "dimension": s.embedder.Dimension()}); err != nil {
			return "", err
		}
	}
	if err := addStep("filter", map[string]any{"max_risk_level": q.MaxRiskLevel, "source_types": q.SourceTypes}); err != nil {
		return "", err
	}
	if s.reranker != nil {
		if err := addStep("rerank", map[string]any{"top_k": q.topK()}); err != nil {
			return "", err
		}
	}
	return plan.ID, nil
}

// SetMetrics wires the optional Prometheus surface (RetrievalLatency).
func (s *Service) SetMetrics(m *observability.Metrics) { s.metrics = m }

// Execute runs the hybrid pipeline and persists results under the plan.
func (s *Service) Execute(ctx context.Context, planID string, q Query) ([]Result, error) {
	ctx, span := observability.Tracer("retrieval").Start(ctx, "retrieval.execute")
	span.SetAttributes(attribute.String("enterprise_id", q.EnterpriseID), attribute.String("plan_id", planID))
	defer span.End()
	if s.metrics != nil {
		start := time.Now()
		defer func() { s.metrics.RetrievalLatency.Observe(time.Since(start).Seconds()) }()
	}

	index := IndexName(q.EnterpriseID)

	keyword, err := s.client.Search(ctx, index, BuildKeywordQuery(q))
	if err != nil {
		return nil, fmt.Errorf("keyword search: %w", err)
	}

	var vectorHits []Hit
	if s.embedder != nil {
		vecs, err := s.embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			return nil, fmt.Errorf("query embedding: %w", err)
		}
		vres, err := s.client.Search(ctx, index, BuildVectorQuery(q, vecs[0]))
		if err != nil {
			return nil, fmt.Errorf("vector search: %w", err)
		}
		vectorHits = vres.Hits
	}

	fused := RRFMerge(keyword.Hits, vectorHits, q.topK())

	results := make([]Result, 0, len(fused))
	for _, h := range fused {
		var doc IndexDocument
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return nil, fmt.Errorf("decode hit %s: %w", h.ID, err)
		}
		snippet := doc.SanitizedSnippet
		if snippet == "" {
			snippet = doc.SummaryText
		}
		results = append(results, Result{
			DocID: h.ID, SpaceID: doc.SpaceID, SourceType: doc.SourceType,
			Snippet: truncateRunes(snippet, 1000), EvidencePointerID: doc.EvidencePointerID,
			Score: h.Score,
		})
	}

	if s.reranker != nil && len(results) > 1 {
		docs := make([]string, len(results))
		for i, r := range results {
			docs[i] = r.Snippet
		}
		ranked, err := s.reranker.Rerank(ctx, q.Text, docs, q.topK())
		if err != nil {
			return nil, fmt.Errorf("rerank: %w", err)
		}
		reordered := make([]Result, 0, len(ranked))
		for _, item := range ranked {
			if item.Index < 0 || item.Index >= len(results) {
				return nil, fmt.Errorf("rerank returned out-of-range index %d", item.Index)
			}
			r := results[item.Index]
			r.Score = item.Score
			reordered = append(reordered, r)
		}
		results = reordered
	}

	for _, r := range results {
		if err := s.store.InsertRetrievalResult(ctx, db.InsertRetrievalResultParams{
			PlanID:            planID,
			EvidencePointerID: pgTextOrNull(r.EvidencePointerID),
			DocRef:            r.DocID,
			Score:             float32(r.Score),
			Snippet:           r.Snippet,
		}); err != nil {
			return nil, fmt.Errorf("persist result: %w", err)
		}
	}
	return results, nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func pgTextOrNull(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
