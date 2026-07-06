package parsergateway

import (
	"context"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
)

type stubProvider struct {
	id    string
	types []string
}

func (s stubProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID: s.id, Capabilities: []string{"pdf.layout"},
		InputTypes: s.types, OutputSchema: sdkparser.OutputSchemaV1,
		MaxFileSizeMB: 1, SupportsPrivate: true,
	}
}

func (s stubProvider) Parse(context.Context, ParseInput) (ParseOutput, error) {
	return ParseOutput{Confidence: 1}, nil
}

func TestRegistryResolve(t *testing.T) {
	r, err := NewRegistry(
		stubProvider{id: "docling", types: []string{"application/pdf", "image/png"}},
		stubProvider{id: "asr", types: []string{"audio/*"}},
	)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	p, err := r.Resolve("", "application/pdf")
	if err != nil || p.Descriptor().ProviderID != "docling" {
		t.Fatalf("pdf resolve: %v %v", p, err)
	}
	p, err = r.Resolve("", "audio/mpeg")
	if err != nil || p.Descriptor().ProviderID != "asr" {
		t.Fatalf("wildcard resolve: %v %v", p, err)
	}
	p, err = r.Resolve("asr", "application/pdf")
	if err != nil || p.Descriptor().ProviderID != "asr" {
		t.Fatalf("hint must win: %v %v", p, err)
	}
	if _, err := r.Resolve("", "video/mp4"); err == nil {
		t.Fatal("unmatched content type must fail")
	}
	if _, err := r.Resolve("ghost", "application/pdf"); err == nil {
		t.Fatal("unknown hint must fail")
	}
}

func TestRegistryRejectsDuplicatesAndBadSchema(t *testing.T) {
	if _, err := NewRegistry(
		stubProvider{id: "docling", types: []string{"application/pdf"}},
		stubProvider{id: "docling", types: []string{"image/png"}},
	); err == nil {
		t.Fatal("duplicate provider must fail")
	}
}

func TestGatewayEnforcesSizeLimit(t *testing.T) {
	r, _ := NewRegistry(stubProvider{id: "docling", types: []string{"application/pdf"}})
	g := NewGateway(r)
	_, err := g.Parse(context.Background(), "", ParseInput{
		ArtifactID: "a1", ContentType: "application/pdf",
		Data: make([]byte, 2*1024*1024), // 2MB > 1MB limit
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds provider") {
		t.Fatalf("size limit must fail loud, got %v", err)
	}
}

func TestMarkdownToBlocks(t *testing.T) {
	blocks := markdownToBlocks("# MES 异常工单排查\n\n第一步：确认工单编号。\n第二步：检查设备状态。\n\n| 风险 | 等级 |\n\n## 附录\n")
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != atlasdocument.BlockHeading || blocks[0].Text != "MES 异常工单排查" {
		t.Fatalf("heading: %+v", blocks[0])
	}
	if blocks[1].Type != atlasdocument.BlockText || !strings.Contains(blocks[1].Text, "第二步") {
		t.Fatalf("paragraph merge: %+v", blocks[1])
	}
	if blocks[2].Type != atlasdocument.BlockTable {
		t.Fatalf("table: %+v", blocks[2])
	}
	for i, b := range blocks {
		if b.Order != i {
			t.Fatalf("order broken at %d: %+v", i, b)
		}
	}
}
