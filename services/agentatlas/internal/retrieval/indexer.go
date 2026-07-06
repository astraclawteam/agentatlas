package retrieval

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

const JobTypeIndex = "index_job"

// IndexStore is the persistence surface for indexing (db/generated satisfies it).
type IndexStore interface {
	GetIndexJob(ctx context.Context, id string) (db.IndexJob, error)
	ClaimIndexJob(ctx context.Context, id string) (int64, error)
	UpdateIndexJobStatus(ctx context.Context, arg db.UpdateIndexJobStatusParams) (int64, error)
	UpsertIndexDocument(ctx context.Context, arg db.UpsertIndexDocumentParams) error
	GetAtlasDocument(ctx context.Context, id string) (db.AtlasDocument, error)
	GetArtifact(ctx context.Context, id string) (db.Artifact, error)
	ListDocumentSummaries(ctx context.Context, atlasDocumentID string) ([]db.DocumentSummary, error)
	GetTimelineNode(ctx context.Context, id string) (db.TimelineNode, error)
	GetDreamSummary(ctx context.Context, id string) (db.DreamSummary, error)
}

// Indexer turns index_jobs into per-enterprise OpenSearch documents.
type Indexer struct {
	store    IndexStore
	client   SearchClient
	embedder Embedder // nil => documents indexed without vectors
}

func NewIndexer(store IndexStore, client SearchClient, embedder Embedder) *Indexer {
	return &Indexer{store: store, client: client, embedder: embedder}
}

func (ix *Indexer) RegisterJobHandler(runner *tasks.Runner) error {
	return runner.Register(JobTypeIndex, tasks.Handler{
		Claim: func(ctx context.Context, jobID string) (bool, error) {
			rows, err := ix.store.ClaimIndexJob(ctx, jobID)
			return rows > 0, err
		},
		Execute: ix.runJob,
		Complete: func(ctx context.Context, jobID string, execErr error) error {
			status, msg := "succeeded", ""
			if execErr != nil {
				status, msg = "failed", truncateRunes(execErr.Error(), 1000)
			}
			_, err := ix.store.UpdateIndexJobStatus(ctx, db.UpdateIndexJobStatusParams{
				ID: jobID, Status: status, Error: msg,
			})
			return err
		},
	})
}

func (ix *Indexer) runJob(ctx context.Context, jobID string) error {
	job, err := ix.store.GetIndexJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load index job: %w", err)
	}
	index := IndexName(job.EnterpriseID)
	if err := ix.client.EnsureIndex(ctx, index); err != nil {
		return err
	}

	docs, err := ix.collect(ctx, job)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("index job %s: source %s/%s produced no indexable documents", jobID, job.SourceType, job.SourceID)
	}

	if ix.embedder != nil {
		texts := make([]string, len(docs))
		for i := range docs {
			texts[i] = docs[i].doc.SummaryText
		}
		vecs, err := ix.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("index embeddings: %w", err)
		}
		for i := range docs {
			docs[i].doc.Embedding = vecs[i]
		}
	}

	for _, d := range docs {
		if err := ix.client.Index(ctx, index, d.id, d.doc); err != nil {
			return err
		}
		if err := ix.store.UpsertIndexDocument(ctx, db.UpsertIndexDocumentParams{
			ID: "ixd_" + d.id, EnterpriseID: job.EnterpriseID, IndexName: index,
			SourceType: job.SourceType, SourceID: d.id,
			EvidencePointerID: pgTextOrNull(d.doc.EvidencePointerID),
			OrgVersion:        d.doc.OrgVersion,
		}); err != nil {
			return fmt.Errorf("index registry: %w", err)
		}
	}
	return nil
}

type indexable struct {
	id  string
	doc IndexDocument
}

func (ix *Indexer) collect(ctx context.Context, job db.IndexJob) ([]indexable, error) {
	switch job.SourceType {
	case "document_summary":
		docRow, err := ix.store.GetAtlasDocument(ctx, job.SourceID)
		if err != nil {
			return nil, fmt.Errorf("atlas document %s: %w", job.SourceID, err)
		}
		artifact, err := ix.store.GetArtifact(ctx, docRow.ArtifactID)
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", docRow.ArtifactID, err)
		}
		pointerID := ""
		if artifact.EvidencePointerID.Valid {
			pointerID = artifact.EvidencePointerID.String
		}
		summaries, err := ix.store.ListDocumentSummaries(ctx, docRow.ID)
		if err != nil {
			return nil, err
		}
		var out []indexable
		for _, s := range summaries {
			if s.Level != "display" && s.Level != "retrieval" {
				continue // sealed/detail layers never enter the index
			}
			out = append(out, indexable{
				id: fmt.Sprintf("%s_%s", docRow.ID, s.Level),
				doc: IndexDocument{
					EnterpriseID: docRow.EnterpriseID, SourceType: "document_summary",
					SummaryText: s.SummaryText, SanitizedSnippet: s.SummaryText,
					EvidencePointerID: pointerID,
				},
			})
		}
		return out, nil

	case "timeline_node":
		node, err := ix.store.GetTimelineNode(ctx, job.SourceID)
		if err != nil {
			return nil, fmt.Errorf("timeline node %s: %w", job.SourceID, err)
		}
		t := node.NodeTime.Time
		return []indexable{{
			id: node.ID,
			doc: IndexDocument{
				EnterpriseID: node.EnterpriseID, SpaceID: node.SpaceID,
				OrgScope: node.OrgScope, SourceType: node.SourceType,
				SummaryText: node.SummaryText, SanitizedSnippet: node.SummaryText,
				Tags: node.Tags, NodeTime: timePtr(t),
				EvidencePointerID: textOrEmpty(node.EvidencePointerID),
			},
		}}, nil

	case "dream_summary":
		sum, err := ix.store.GetDreamSummary(ctx, job.SourceID)
		if err != nil {
			return nil, fmt.Errorf("dream summary %s: %w", job.SourceID, err)
		}
		if sum.Layer == "sealed_pointer" {
			return nil, fmt.Errorf("sealed dream summaries must not be indexed (id %s)", sum.ID)
		}
		return []indexable{{
			id: sum.ID,
			doc: IndexDocument{
				EnterpriseID: sum.EnterpriseID, SpaceID: sum.SpaceID,
				SourceType: "dream_summary", SummaryText: sum.SummaryText,
				SanitizedSnippet: sum.SummaryText,
				EvidencePointerID: textOrEmpty(sum.EvidencePointerID),
				NodeTime:          timePtr(sum.CreatedAt.Time),
			},
		}}, nil

	default:
		return nil, fmt.Errorf("unknown index source type %q", job.SourceType)
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func textOrEmpty(v pgtype.Text) string {
	if v.Valid {
		return v.String
	}
	return ""
}
