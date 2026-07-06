package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// Store is the persistence surface for the pipeline (db/generated satisfies it).
type Store interface {
	CreateArtifact(ctx context.Context, arg db.CreateArtifactParams) (db.Artifact, error)
	GetArtifact(ctx context.Context, id string) (db.Artifact, error)
	CreateArtifactJob(ctx context.Context, arg db.CreateArtifactJobParams) (db.ArtifactProcessingJob, error)
	GetArtifactJob(ctx context.Context, id string) (db.ArtifactProcessingJob, error)
	UpdateArtifactJobStatus(ctx context.Context, arg db.UpdateArtifactJobStatusParams) (int64, error)
	InsertArtifactStep(ctx context.Context, arg db.InsertArtifactStepParams) error
	InsertParserProviderRun(ctx context.Context, arg db.InsertParserProviderRunParams) error
	CreateAtlasDocument(ctx context.Context, arg db.CreateAtlasDocumentParams) (db.AtlasDocument, error)
	GetAtlasDocument(ctx context.Context, id string) (db.AtlasDocument, error)
	GetAtlasDocumentByArtifact(ctx context.Context, artifactID string) (db.AtlasDocument, error)
	InsertDocumentBlock(ctx context.Context, arg db.InsertDocumentBlockParams) error
	InsertDocumentSummary(ctx context.Context, arg db.InsertDocumentSummaryParams) (db.DocumentSummary, error)
	CreateEvidencePointer(ctx context.Context, arg db.CreateEvidencePointerParams) (db.EvidencePointer, error)
	CreateIndexJob(ctx context.Context, arg db.CreateIndexJobParams) (db.IndexJob, error)
}

// Objects is the object-storage surface (storage.ObjectStore satisfies it).
type Objects interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ParserGateway parses artifact bytes — the in-process parsergateway.Gateway
// or the HTTP-backed parsergateway.HTTPClient against the parser-gateway
// service; the composition root decides which topology runs.
type ParserGateway interface {
	Parse(ctx context.Context, hint string, in parsergateway.ParseInput) (parsergateway.GatewayResult, error)
}

type Service struct {
	store      Store
	objects    Objects
	gateway    ParserGateway
	runner     *tasks.Runner
	summarizer *Summarizer
}

// NewService wires the artifact pipeline. summarizer may be nil (or
// model-less) — that is the explicit deterministic degraded mode.
func NewService(store Store, objects Objects, gateway ParserGateway, runner *tasks.Runner, summarizer *Summarizer) *Service {
	return &Service{store: store, objects: objects, gateway: gateway, runner: runner, summarizer: summarizer}
}

// RegisterJobHandler wires the artifact job type into the task runner.
func (s *Service) RegisterJobHandler() error {
	return s.runner.Register(JobTypeArtifact, tasks.Handler{
		Claim: func(ctx context.Context, jobID string) (bool, error) {
			job, err := s.store.GetArtifactJob(ctx, jobID)
			if err != nil {
				return false, err
			}
			if job.Status != "pending" {
				return false, nil
			}
			rows, err := s.store.UpdateArtifactJobStatus(ctx, db.UpdateArtifactJobStatusParams{
				ID: jobID, Status: "running", Error: "",
			})
			return rows > 0, err
		},
		Execute: s.runJob,
		Complete: func(ctx context.Context, jobID string, execErr error) error {
			status, msg := "succeeded", ""
			if execErr != nil {
				status, msg = "failed", truncateRunes(execErr.Error(), 1000)
			}
			_, err := s.store.UpdateArtifactJobStatus(ctx, db.UpdateArtifactJobStatusParams{
				ID: jobID, Status: status, Error: msg,
			})
			return err
		},
	})
}

// IngestUpload stores an uploaded file in object storage and registers the
// artifact row (metadata only — raw bytes never touch PostgreSQL).
func (s *Service) IngestUpload(ctx context.Context, enterpriseID, filename, contentType string, data []byte) (db.Artifact, error) {
	id := newID("art")
	objectKey := fmt.Sprintf("artifacts/%s/%s", enterpriseID, id)
	if err := s.objects.Put(ctx, objectKey, contentType, data); err != nil {
		return db.Artifact{}, fmt.Errorf("store artifact: %w", err)
	}
	return s.store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: id, EnterpriseID: enterpriseID, Filename: filename,
		ContentType: contentType, ObjectKey: objectKey,
		SourceHash: hashBytes(data), SizeBytes: int64(len(data)),
	})
}

// CreateJob registers and dispatches a processing job.
func (s *Service) CreateJob(ctx context.Context, enterpriseID, artifactID, parserHint string) (db.ArtifactProcessingJob, error) {
	if parserHint == "" {
		parserHint = "auto"
	}
	job, err := s.store.CreateArtifactJob(ctx, db.CreateArtifactJobParams{
		ID: newID("job"), EnterpriseID: enterpriseID, ArtifactID: artifactID,
		Status: "pending", ParserHint: parserHint,
	})
	if err != nil {
		return db.ArtifactProcessingJob{}, err
	}
	if err := s.runner.Enqueue(ctx, JobTypeArtifact, job.ID); err != nil {
		return db.ArtifactProcessingJob{}, fmt.Errorf("dispatch job: %w", err)
	}
	return job, nil
}

func (s *Service) step(ctx context.Context, jobID, step, status string, detail map[string]any) {
	raw, _ := json.Marshal(detail)
	if raw == nil {
		raw = []byte(`{}`)
	}
	_ = s.store.InsertArtifactStep(ctx, db.InsertArtifactStepParams{
		JobID: jobID, Step: step, Status: status, Detail: raw,
	})
}

// runJob executes the pipeline for one claimed job.
func (s *Service) runJob(ctx context.Context, jobID string) error {
	job, err := s.store.GetArtifactJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load job: %w", err)
	}
	artifact, err := s.store.GetArtifact(ctx, job.ArtifactID)
	if err != nil {
		return fmt.Errorf("load artifact: %w", err)
	}
	data, err := s.objects.Get(ctx, artifact.ObjectKey)
	if err != nil {
		return fmt.Errorf("fetch artifact bytes: %w", err)
	}

	s.step(ctx, jobID, "parse", "running", map[string]any{"parser_hint": job.ParserHint})
	result, err := s.gateway.Parse(ctx, job.ParserHint, parsergateway.ParseInput{
		EnterpriseID: artifact.EnterpriseID,
		ArtifactID:   artifact.ID,
		Filename:     artifact.Filename,
		ContentType:  artifact.ContentType,
		Data:         data,
	})
	if err != nil {
		s.step(ctx, jobID, "parse", "failed", map[string]any{"error": err.Error()})
		return err
	}
	_ = s.store.InsertParserProviderRun(ctx, db.InsertParserProviderRunParams{
		JobID: jobID, ProviderID: result.Output.ProviderID, Status: "succeeded",
		LatencyMs: int32(result.LatencyMS), Confidence: float32(result.Output.Confidence),
	})
	s.step(ctx, jobID, "parse", "succeeded", map[string]any{"provider": result.Output.ProviderID})

	summaries, summarySource, err := s.summarizer.Summarize(ctx, result.Output)
	if err != nil {
		s.step(ctx, jobID, "summarize", "failed", map[string]any{"error": err.Error()})
		return fmt.Errorf("summarize: %w", err)
	}
	s.step(ctx, jobID, "summarize", "succeeded", map[string]any{
		"source": summarySource, "summaries": len(summaries),
	})

	// Assemble the full AtlasDocument and seal it into object storage.
	docID := newID("doc")
	pointer, err := s.store.CreateEvidencePointer(ctx, BuildEvidencePointer(
		artifact.EnterpriseID, "artifact", artifact.ObjectKey, "object-storage", artifact.SourceHash, nil))
	if err != nil {
		return fmt.Errorf("evidence pointer: %w", err)
	}
	fullDoc := atlasdocument.AtlasDocument{
		AtlasDocumentID: docID,
		ArtifactID:      artifact.ID,
		SourceHash:      artifact.SourceHash,
		ContentType:     artifact.ContentType,
		Provider:        result.Output.ProviderID,
		Confidence:      result.Output.Confidence,
		Blocks:          result.Output.Blocks,
		Tables:          result.Output.Tables,
		Images:          result.Output.Images,
		AudioSegments:   result.Output.AudioSegments,
		VideoSegments:   result.Output.VideoSegments,
		Summaries:       summaries,
		EvidencePointers: []atlasdocument.EvidencePointer{{
			EvidenceID:   pointer.ID,
			EnterpriseID: pointer.EnterpriseID,
			ResourceType: pointer.ResourceType,
			ResourceRef:  pointer.ResourceRef,
			SourceSystem: pointer.SourceSystem,
			ContentHash:  pointer.ContentHash,
		}},
	}
	rawDoc, err := json.Marshal(fullDoc)
	if err != nil {
		return fmt.Errorf("encode atlas document: %w", err)
	}
	docKey := fmt.Sprintf("atlasdocs/%s/%s.json", artifact.EnterpriseID, docID)
	if err := s.objects.Put(ctx, docKey, "application/json", rawDoc); err != nil {
		return fmt.Errorf("store atlas document: %w", err)
	}
	s.step(ctx, jobID, "store", "succeeded", map[string]any{"object_key": docKey})

	// Metadata tables get hashes, sanitized excerpts, and summaries only.
	if _, err := s.store.CreateAtlasDocument(ctx, db.CreateAtlasDocumentParams{
		ID: docID, EnterpriseID: artifact.EnterpriseID, ArtifactID: artifact.ID,
		SourceHash: artifact.SourceHash, ContentType: artifact.ContentType,
		Provider: result.Output.ProviderID, ObjectKey: docKey,
		Confidence: float32(result.Output.Confidence),
	}); err != nil {
		return fmt.Errorf("atlas document row: %w", err)
	}
	for _, b := range result.Output.Blocks {
		if err := s.store.InsertDocumentBlock(ctx, db.InsertDocumentBlockParams{
			AtlasDocumentID: docID, BlockID: b.BlockID, BlockType: string(b.Type),
			Page: int32OrNull(b.Page), BlockOrder: int32(b.Order),
			TextHash:         hashBytes([]byte(b.Text)),
			SanitizedExcerpt: truncateRunes(b.Text, maxExcerptLen),
		}); err != nil {
			return fmt.Errorf("document block %s: %w", b.BlockID, err)
		}
	}
	for _, sum := range fullDoc.Summaries {
		if _, err := s.store.InsertDocumentSummary(ctx, db.InsertDocumentSummaryParams{
			AtlasDocumentID: docID, Level: string(sum.Level), Ref: sum.Ref,
			SummaryText: truncateRunes(sum.Text, maxSummaryLen),
		}); err != nil {
			return fmt.Errorf("document summary: %w", err)
		}
	}

	// Queue indexing (executor lands with Goal 10).
	if _, err := s.store.CreateIndexJob(ctx, db.CreateIndexJobParams{
		ID: newID("idx"), EnterpriseID: artifact.EnterpriseID,
		SourceType: "document_summary", SourceID: docID, Status: "pending",
	}); err != nil {
		return fmt.Errorf("index job: %w", err)
	}
	s.step(ctx, jobID, "index_enqueue", "succeeded", map[string]any{"atlas_document_id": docID})
	return nil
}

// buildSummaries derives structural display/retrieval summaries. LLM-grade
// summaries come from the transform.summarize node; these keep the document
// discoverable even without model access.
func buildSummaries(out parsergateway.ParseOutput) []atlasdocument.Summary {
	var headings, texts []string
	for _, b := range out.Blocks {
		switch b.Type {
		case atlasdocument.BlockHeading:
			headings = append(headings, b.Text)
		case atlasdocument.BlockText:
			texts = append(texts, b.Text)
		}
	}
	for _, seg := range out.AudioSegments {
		texts = append(texts, seg.Text)
	}

	display := ""
	if len(headings) > 0 {
		display = headings[0]
	} else if len(texts) > 0 {
		display = truncateRunes(texts[0], 120)
	}
	retrieval := strings.Join(headings, "；")
	if retrieval == "" && len(texts) > 0 {
		retrieval = truncateRunes(strings.Join(texts, " "), 600)
	}

	var summaries []atlasdocument.Summary
	if display != "" {
		summaries = append(summaries, atlasdocument.Summary{
			SummaryID: newID("sum"), Level: atlasdocument.SummaryDisplay, Text: truncateRunes(display, 200),
		})
	}
	if retrieval != "" {
		summaries = append(summaries, atlasdocument.Summary{
			SummaryID: newID("sum"), Level: atlasdocument.SummaryRetrieval, Text: truncateRunes(retrieval, 1000),
		})
	}
	return summaries
}

func int32OrNull(v int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(v), Valid: v > 0}
}
