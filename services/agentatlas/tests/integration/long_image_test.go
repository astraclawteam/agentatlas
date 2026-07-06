// Long-image integration: real docling OCR tiles + a real VLM through
// llmrouter produce ordered region summaries. Run:
//
//	ATLAS_PARSER_SIDECARS=1 LLMROUTER_BASE_URL=... LLMROUTER_API_KEY=... \
//	LLMROUTER_VLM_MODEL=doubao-seed-2-0-lite \
//	  go test ./tests/integration -run TestLongImage
package integration

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

type memObjectsIT struct{ data map[string][]byte }

func (m *memObjectsIT) Put(_ context.Context, key, _ string, data []byte) error {
	m.data[key] = data
	return nil
}
func (m *memObjectsIT) Get(_ context.Context, key string) ([]byte, error) { return m.data[key], nil }

func TestLongImage(t *testing.T) {
	if os.Getenv("ATLAS_PARSER_SIDECARS") != "1" {
		t.Skip("set ATLAS_PARSER_SIDECARS=1 (docling sidecar) for the OCR leg")
	}
	baseURL := os.Getenv("LLMROUTER_BASE_URL")
	apiKey := os.Getenv("LLMROUTER_API_KEY")
	vlmModel := os.Getenv("LLMROUTER_VLM_MODEL")
	if baseURL == "" || apiKey == "" || vlmModel == "" {
		t.Skip("set LLMROUTER_BASE_URL, LLMROUTER_API_KEY, LLMROUTER_VLM_MODEL for the VLM leg")
	}

	doclingURL := envOr("ATLAS_TEST_DOCLING_URL", "http://localhost:5001")
	if !sidecarUp(doclingURL) {
		t.Skipf("docling sidecar unreachable at %s", doclingURL)
	}
	registry, err := parsergateway.NewRegistry(parsergateway.NewDoclingProvider(doclingURL))
	if err != nil {
		t.Fatal(err)
	}
	m, err := llmroutermodel.New(llmroutermodel.Config{
		BaseURL: baseURL, APIKey: apiKey, DefaultModel: vlmModel, Timeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// tall synthetic image with colored bands (charts-like, sparse text)
	img := image.NewRGBA(image.Rect(0, 0, 480, 2600))
	for y := 0; y < 2600; y++ {
		for x := 0; x < 480; x++ {
			img.Set(x, y, color.RGBA{uint8((y / 200) * 20), 90, 160, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	p := &artifacts.LongImageProcessor{
		OCR: artifacts.GatewayTileOCR{Gateway: parsergateway.NewGateway(registry)},
		VLM: func(ctx context.Context, data []byte, mime, prompt string) (string, error) {
			return m.DescribeImage(ctx, vlmModel, prompt, data, mime)
		},
		Objects: &memObjectsIT{data: map[string][]byte{}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	res, err := p.Process(ctx, "ent_it", "art_long", buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(res.Regions) < 2 {
		t.Fatalf("regions = %d, want tiled >=2", len(res.Regions))
	}
	for i, r := range res.Regions {
		if r.Index != i {
			t.Fatalf("reading order broken at %d", i)
		}
	}
	if res.Display == "" {
		t.Fatal("display summary empty")
	}
	t.Logf("long image: %d regions source=%s display=%q", len(res.Regions), res.Source, res.Display)
}
