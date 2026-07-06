package artifacts

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// syntheticLongImage renders a tall image forcing multiple tiles.
func syntheticLongImage(t *testing.T, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, height))
	for y := 0; y < height; y++ {
		for x := 0; x < 400; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// scriptedOCR yields rich text for even tiles and sparse text for odd tiles
// (odd tiles become "key regions" for the VLM).
type scriptedOCR struct{ calls int }

func (s *scriptedOCR) OCRTile(_ context.Context, _ []byte, _ string) (string, error) {
	idx := s.calls
	s.calls++
	if idx%2 == 1 {
		return "图", nil // sparse -> VLM key region
	}
	return fmt.Sprintf("第 %d 区文本：MES 异常工单排查操作说明，覆盖工单编号确认、异常原因排查、限流风险上报、管理员确认与归档的完整步骤及各环节注意事项与责任人说明。", idx), nil
}

func TestLongImageOCRVLMReadingOrder(t *testing.T) {
	objects := &memObjectsLI{data: map[string][]byte{}}
	var vlmCalls []int
	ocr := &scriptedOCR{}
	p := &LongImageProcessor{
		OCR: ocr,
		VLM: func(_ context.Context, _ []byte, _, _ string) (string, error) {
			vlmCalls = append(vlmCalls, ocr.calls-1)
			return "该区域是一张流程图。", nil
		},
		Objects: objects,
	}

	// 3000px tall / (1024-128) step -> 4 tiles
	res, err := p.Process(context.Background(), "ent_1", "art_img", syntheticLongImage(t, 3000), "image/png")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(res.Regions) < 3 {
		t.Fatalf("regions = %d, want >=3 (tiled)", len(res.Regions))
	}
	// reading order = ascending tile index
	for i, r := range res.Regions {
		if r.Index != i {
			t.Fatalf("region %d has index %d — reading order broken", i, r.Index)
		}
	}
	// sparse tiles got VLM descriptions; rich tiles did not
	for _, r := range res.Regions {
		sparse := r.Index%2 == 1
		if sparse && r.VLMDescription == "" {
			t.Fatalf("key region %d missing VLM description", r.Index)
		}
		if !sparse && r.VLMDescription != "" {
			t.Fatalf("rich region %d needlessly sent to VLM", r.Index)
		}
	}
	if res.Source != "ocr+vlm" {
		t.Fatalf("source = %q", res.Source)
	}

	// full OCR sealed to object storage; metadata excerpts capped
	sealed, ok := objects.data[res.OCRObjectKey]
	if !ok || !strings.Contains(string(sealed), "--- tile 0 ---") {
		t.Fatalf("full OCR not sealed: key=%s", res.OCRObjectKey)
	}
	if n := len([]rune(res.Display)); n > 200 {
		t.Fatalf("display cap violated: %d", n)
	}
	for _, r := range res.Regions {
		if len([]rune(r.OCRExcerpt)) > maxExcerptLen {
			t.Fatalf("region excerpt exceeds sanitized cap")
		}
	}
}

func TestLongImageOCROnlyDegradedMode(t *testing.T) {
	objects := &memObjectsLI{data: map[string][]byte{}}
	p := &LongImageProcessor{OCR: &scriptedOCR{}, VLM: nil, Objects: objects}
	res, err := p.Process(context.Background(), "ent_1", "art_img2", syntheticLongImage(t, 2200), "image/png")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Source != "ocr_only" {
		t.Fatalf("source = %q, want ocr_only", res.Source)
	}
	for _, r := range res.Regions {
		if r.VLMDescription != "" {
			t.Fatal("no VLM configured but description present")
		}
	}
}

func TestLongImageFailsLoudWithoutDeps(t *testing.T) {
	p := &LongImageProcessor{}
	if _, err := p.Process(context.Background(), "e", "a", nil, "image/png"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing OCR must fail loud, got %v", err)
	}
}

type memObjectsLI struct{ data map[string][]byte }

func (m *memObjectsLI) Put(_ context.Context, key, _ string, data []byte) error {
	m.data[key] = data
	return nil
}
func (m *memObjectsLI) Get(_ context.Context, key string) ([]byte, error) {
	d, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	return d, nil
}
