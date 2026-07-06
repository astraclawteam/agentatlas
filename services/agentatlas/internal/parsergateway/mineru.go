package parsergateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
)

// MinerUProvider talks to the mineru-api sidecar (POST /file_parse) for
// complex PDFs, long documents, and layout/table restoration.
type MinerUProvider struct {
	baseURL string
	http    *http.Client
}

func NewMinerUProvider(baseURL string) *MinerUProvider {
	return &MinerUProvider{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Minute}}
}

func (p *MinerUProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID:      "mineru",
		Capabilities:    []string{sdkparser.CapPDFLayout, sdkparser.CapPDFTable, sdkparser.CapImageOCR},
		InputTypes:      []string{"application/pdf"},
		OutputSchema:    sdkparser.OutputSchemaV1,
		MaxFileSizeMB:   500,
		SupportsPrivate: true,
	}
}

type mineruResponse struct {
	MDContent string `json:"md_content"`
	Error     string `json:"error"`
}

func (p *MinerUProvider) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	var resp mineruResponse
	err := postMultipart(ctx, p.http, p.baseURL+"/file_parse",
		map[string]string{"return_md": "true"},
		"file", nonEmpty(in.Filename, in.ArtifactID+".pdf"), in.Data, &resp)
	if err != nil {
		return ParseOutput{}, err
	}
	if resp.Error != "" {
		return ParseOutput{}, fmt.Errorf("mineru: %s", resp.Error)
	}
	if strings.TrimSpace(resp.MDContent) == "" {
		return ParseOutput{}, fmt.Errorf("mineru returned empty document")
	}
	return ParseOutput{Blocks: markdownToBlocks(resp.MDContent), Confidence: 0.92}, nil
}
