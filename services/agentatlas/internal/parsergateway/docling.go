package parsergateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
)

// DoclingProvider talks to docling-serve (POST /v1alpha/convert/source).
type DoclingProvider struct {
	baseURL string
	http    *http.Client
}

func NewDoclingProvider(baseURL string) *DoclingProvider {
	return &DoclingProvider{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 5 * time.Minute}}
}

func (p *DoclingProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID:      "docling",
		Capabilities:    []string{sdkparser.CapPDFLayout, sdkparser.CapPDFTable, sdkparser.CapImageOCR, sdkparser.CapOfficeParse},
		InputTypes: []string{
			"application/pdf", "image/png", "image/jpeg", "text/markdown", "text/plain", "text/html",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		},
		OutputSchema:    sdkparser.OutputSchemaV1,
		MaxFileSizeMB:   200,
		SupportsPrivate: true,
	}
}

type doclingRequest struct {
	Options     map[string]any      `json:"options"`
	FileSources []doclingFileSource `json:"file_sources"`
}

type doclingFileSource struct {
	Base64String string `json:"base64_string"`
	Filename     string `json:"filename"`
}

type doclingResponse struct {
	Status   string `json:"status"`
	Document struct {
		MDContent string `json:"md_content"`
	} `json:"document"`
	Errors []any `json:"errors"`
}

func (p *DoclingProvider) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	req := doclingRequest{
		Options: map[string]any{"to_formats": []string{"md"}, "image_export_mode": "placeholder"},
		FileSources: []doclingFileSource{{
			Base64String: base64.StdEncoding.EncodeToString(in.Data),
			Filename:     nonEmpty(in.Filename, in.ArtifactID),
		}},
	}
	var resp doclingResponse
	if err := postJSON(ctx, p.http, p.baseURL+"/v1alpha/convert/source", req, &resp); err != nil {
		return ParseOutput{}, err
	}
	if resp.Status != "" && resp.Status != "success" {
		return ParseOutput{}, fmt.Errorf("docling status %q (errors: %v)", resp.Status, resp.Errors)
	}
	if strings.TrimSpace(resp.Document.MDContent) == "" {
		return ParseOutput{}, fmt.Errorf("docling returned empty document")
	}
	return ParseOutput{Blocks: markdownToBlocks(resp.Document.MDContent), Confidence: 0.9}, nil
}

// markdownToBlocks converts exported markdown into ordered heading/text/table
// blocks — enough structure for SOP extraction and summaries downstream.
func markdownToBlocks(md string) []atlasdocument.Block {
	var blocks []atlasdocument.Block
	order := 0
	appendBlock := func(t atlasdocument.BlockType, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		blocks = append(blocks, atlasdocument.Block{
			BlockID: fmt.Sprintf("b%d", order),
			Type:    t,
			Order:   order,
			Text:    text,
		})
		order++
	}
	var paragraph []string
	flush := func() {
		if len(paragraph) > 0 {
			appendBlock(atlasdocument.BlockText, strings.Join(paragraph, "\n"))
			paragraph = nil
		}
	}
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#"):
			flush()
			appendBlock(atlasdocument.BlockHeading, strings.TrimLeft(trimmed, "# "))
		case strings.HasPrefix(trimmed, "|"):
			flush()
			appendBlock(atlasdocument.BlockTable, trimmed)
		case trimmed == "":
			flush()
		default:
			paragraph = append(paragraph, trimmed)
		}
	}
	flush()
	return blocks
}

func nonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
