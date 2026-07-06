package artifacts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// ParseArtifact runs the full parse pipeline for one already-ingested artifact
// SYNCHRONOUSLY (job row + steps recorded like the async path) and returns the
// atlas document id. Workflow parser.* nodes use this; bulk ingestion keeps
// the async CreateJob + worker path.
func (s *Service) ParseArtifact(ctx context.Context, enterpriseID, artifactID, parserHint string) (string, error) {
	if parserHint == "" {
		parserHint = "auto"
	}
	job, err := s.store.CreateArtifactJob(ctx, db.CreateArtifactJobParams{
		ID: newID("job"), EnterpriseID: enterpriseID, ArtifactID: artifactID,
		Status: "running", ParserHint: parserHint,
	})
	if err != nil {
		return "", fmt.Errorf("create parse job: %w", err)
	}
	execErr := s.runJob(ctx, job.ID)
	status, msg := "succeeded", ""
	if execErr != nil {
		status, msg = "failed", truncateRunes(execErr.Error(), 1000)
	}
	if _, err := s.store.UpdateArtifactJobStatus(ctx, db.UpdateArtifactJobStatusParams{
		ID: job.ID, Status: status, Error: msg,
	}); err != nil {
		return "", fmt.Errorf("finalize parse job: %w", err)
	}
	if execErr != nil {
		return "", execErr
	}
	doc, err := s.store.GetAtlasDocumentByArtifact(ctx, artifactID)
	if err != nil {
		return "", fmt.Errorf("locate parsed document for %s: %w", artifactID, err)
	}
	return doc.ID, nil
}

// loadFullDocument fetches the sealed AtlasDocument from object storage.
func (s *Service) loadFullDocument(ctx context.Context, atlasDocumentID string) (atlasdocument.AtlasDocument, error) {
	row, err := s.store.GetAtlasDocument(ctx, atlasDocumentID)
	if err != nil {
		return atlasdocument.AtlasDocument{}, fmt.Errorf("atlas document %s: %w", atlasDocumentID, err)
	}
	raw, err := s.objects.Get(ctx, row.ObjectKey)
	if err != nil {
		return atlasdocument.AtlasDocument{}, fmt.Errorf("sealed document %s: %w", row.ObjectKey, err)
	}
	var doc atlasdocument.AtlasDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return atlasdocument.AtlasDocument{}, fmt.Errorf("decode sealed document: %w", err)
	}
	return doc, nil
}

// LoadSOPDocument builds the sanitized agent context (title/summary/sections)
// from the sealed AtlasDocument (workflow transform.extract_sop node).
func (s *Service) LoadSOPDocument(ctx context.Context, atlasDocumentID string) (agent.SOPDocument, error) {
	doc, err := s.loadFullDocument(ctx, atlasDocumentID)
	if err != nil {
		return agent.SOPDocument{}, err
	}
	out := agent.SOPDocument{}
	for _, sum := range doc.Summaries {
		if sum.Level == atlasdocument.SummaryDisplay && out.Summary == "" {
			out.Summary = sum.Text
		}
	}
	for _, b := range doc.Blocks {
		switch b.Type {
		case atlasdocument.BlockHeading:
			if out.Title == "" {
				out.Title = b.Text
			}
			out.Sections = append(out.Sections, b.Text)
		case atlasdocument.BlockText:
			out.Sections = append(out.Sections, b.Text)
		}
	}
	if len(out.Sections) == 0 && out.Summary == "" {
		return agent.SOPDocument{}, fmt.Errorf("atlas document %s has no textual content for SOP extraction", atlasDocumentID)
	}
	return out, nil
}

// LoadParseOutput reconstructs the provider-normalized fragment from the
// sealed AtlasDocument (workflow transform.summarize node).
func (s *Service) LoadParseOutput(ctx context.Context, atlasDocumentID string) (parsergateway.ParseOutput, error) {
	doc, err := s.loadFullDocument(ctx, atlasDocumentID)
	if err != nil {
		return parsergateway.ParseOutput{}, err
	}
	return parsergateway.ParseOutput{
		ProviderID:    doc.Provider,
		Blocks:        doc.Blocks,
		Tables:        doc.Tables,
		Images:        doc.Images,
		AudioSegments: doc.AudioSegments,
		VideoSegments: doc.VideoSegments,
		Confidence:    doc.Confidence,
	}, nil
}
