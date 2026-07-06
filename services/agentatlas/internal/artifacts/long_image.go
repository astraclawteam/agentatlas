package artifacts

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

// Long images are never sent to a VLM whole: they are tiled top-to-bottom
// with overlap so no text line is cut, then OCR/VLM runs per tile and the
// reading order is reconstructed by tile index.
const (
	tileHeight  = 1024
	tileOverlap = 128
)

type Tile struct {
	Index  int `json:"index"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// TilingPlan slices an image of the given size into overlapping tiles in
// reading order (top to bottom).
func TilingPlan(width, height int) ([]Tile, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid image size %dx%d", width, height)
	}
	if height <= tileHeight {
		return []Tile{{Index: 0, X: 0, Y: 0, Width: width, Height: height}}, nil
	}
	var tiles []Tile
	step := tileHeight - tileOverlap
	for y, i := 0, 0; y < height; y, i = y+step, i+1 {
		h := tileHeight
		if y+h > height {
			h = height - y
		}
		tiles = append(tiles, Tile{Index: i, X: 0, Y: y, Width: width, Height: h})
		if y+tileHeight >= height {
			break
		}
	}
	return tiles, nil
}

// TilingPlanFromImage reads only the image header (no full decode).
func TilingPlanFromImage(data []byte) ([]Tile, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}
	return TilingPlan(cfg.Width, cfg.Height)
}
