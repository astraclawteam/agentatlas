package artifacts

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"strings"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// TileOCR extracts text from one tile image (production: the docling image
// route through the parser gateway).
type TileOCR interface {
	OCRTile(ctx context.Context, imageData []byte, mimeType string) (string, error)
}

// RegionDescriber describes one image region (production: a VLM through the
// llmrouter multimodal route — llmroutermodel.Client.DescribeImage bound to
// the deployment's vision model).
type RegionDescriber func(ctx context.Context, imageData []byte, mimeType, prompt string) (string, error)

// GatewayTileOCR adapts the parser gateway's image capability to TileOCR.
type GatewayTileOCR struct {
	Gateway ParserGateway
	Hint    string // parser hint for image OCR; empty = auto (content-type routed)
}

func (g GatewayTileOCR) OCRTile(ctx context.Context, imageData []byte, mimeType string) (string, error) {
	result, err := g.Gateway.Parse(ctx, g.Hint, ParseInputForTile(imageData, mimeType))
	if err != nil {
		return "", err
	}
	var texts []string
	for _, b := range result.Output.Blocks {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// LongImageRegion is one tile's understanding in reading order.
type LongImageRegion struct {
	Index          int    `json:"index"`
	OCRExcerpt     string `json:"ocr_excerpt"`               // sanitized excerpt only
	VLMDescription string `json:"vlm_description,omitempty"` // key regions only
}

// LongImageResult carries sanitized summaries + the object-storage key of the
// full OCR text. Full text NEVER lands in metadata tables.
type LongImageResult struct {
	Regions      []LongImageRegion `json:"regions"`
	Display      string            `json:"display"`
	Retrieval    string            `json:"retrieval"`
	OCRObjectKey string            `json:"ocr_object_key"`
	Source       string            `json:"source"` // "ocr+vlm" | "ocr_only"
}

// sparseOCRThreshold: tiles whose OCR yields fewer runes than this are "key
// regions" — likely charts/figures — and get a VLM description.
const sparseOCRThreshold = 40

// LongImageProcessor runs tiling → per-tile OCR → per-key-region VLM →
// reading-order reconstruction. A single VLM call never receives the whole
// image — only tiles. Nil vlm = explicit OCR-only degraded mode.
type LongImageProcessor struct {
	OCR     TileOCR
	VLM     RegionDescriber
	Objects Objects
}

// Process understands one long image for (enterpriseID, artifactID).
func (p *LongImageProcessor) Process(ctx context.Context, enterpriseID, artifactID string, imageData []byte, mimeType string) (LongImageResult, error) {
	if p.OCR == nil {
		return LongImageResult{}, fmt.Errorf("long image: OCR dependency not configured")
	}
	if p.Objects == nil {
		return LongImageResult{}, fmt.Errorf("long image: object storage required (full OCR is sealed, never in metadata)")
	}
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return LongImageResult{}, fmt.Errorf("decode long image: %w", err)
	}
	bounds := img.Bounds()
	tiles, err := TilingPlan(bounds.Dx(), bounds.Dy())
	if err != nil {
		return LongImageResult{}, err
	}

	var fullOCR strings.Builder
	regions := make([]LongImageRegion, 0, len(tiles))
	source := "ocr_only"
	for _, tile := range tiles {
		tileBytes, err := cropTile(img, tile)
		if err != nil {
			return LongImageResult{}, fmt.Errorf("tile %d: %w", tile.Index, err)
		}
		text, err := p.OCR.OCRTile(ctx, tileBytes, "image/png")
		if err != nil {
			return LongImageResult{}, fmt.Errorf("tile %d ocr: %w", tile.Index, err)
		}
		fmt.Fprintf(&fullOCR, "--- tile %d ---\n%s\n", tile.Index, text)

		region := LongImageRegion{Index: tile.Index, OCRExcerpt: truncateRunes(text, maxExcerptLen)}
		// Key regions (sparse OCR = probably a figure/chart) get a VLM pass.
		if p.VLM != nil && len([]rune(strings.TrimSpace(text))) < sparseOCRThreshold {
			desc, err := p.VLM(ctx, tileBytes, "image/png",
				"这是一张长图的一个分块区域。请用中文简要描述该区域的内容与版面结构（如图表/表格/流程），不超过80字，不得虚构。")
			if err != nil {
				return LongImageResult{}, fmt.Errorf("tile %d vlm: %w", tile.Index, err)
			}
			region.VLMDescription = truncateRunes(strings.TrimSpace(desc), 200)
			source = "ocr+vlm"
		}
		regions = append(regions, region)
	}

	// Full OCR text is sealed to object storage; metadata keeps excerpts only.
	ocrKey := fmt.Sprintf("longimages/%s/%s/ocr.txt", enterpriseID, artifactID)
	if err := p.Objects.Put(ctx, ocrKey, "text/plain", []byte(fullOCR.String())); err != nil {
		return LongImageResult{}, fmt.Errorf("seal full ocr: %w", err)
	}

	var display, retrieval strings.Builder
	for _, r := range regions {
		piece := r.OCRExcerpt
		if piece == "" {
			piece = r.VLMDescription
		}
		if piece == "" {
			continue
		}
		if display.Len() > 0 {
			display.WriteString("；")
			retrieval.WriteString("；")
		}
		display.WriteString(strings.ReplaceAll(piece, "\n", " "))
		retrieval.WriteString(strings.ReplaceAll(piece, "\n", " "))
		if r.VLMDescription != "" && r.OCRExcerpt != "" {
			retrieval.WriteString("（" + r.VLMDescription + "）")
		}
	}
	return LongImageResult{
		Regions:      regions,
		Display:      truncateRunes(display.String(), 200),
		Retrieval:    truncateRunes(retrieval.String(), 1000),
		OCRObjectKey: ocrKey,
		Source:       source,
	}, nil
}

// cropTile renders one tile into a standalone PNG.
func cropTile(img image.Image, t Tile) ([]byte, error) {
	rect := image.Rect(0, 0, t.Width, t.Height)
	out := image.NewRGBA(rect)
	draw.Draw(out, rect, img, image.Pt(t.X, img.Bounds().Min.Y+t.Y), draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseInputForTile builds the gateway input for one tile.
func ParseInputForTile(imageData []byte, mimeType string) parsergateway.ParseInput {
	return parsergateway.ParseInput{
		Filename:    "tile.png",
		ContentType: mimeType,
		Data:        imageData,
	}
}
