package artifacts

import "testing"

func TestTilingPlanSmallImageSingleTile(t *testing.T) {
	tiles, err := TilingPlan(800, 600)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(tiles) != 1 || tiles[0].Height != 600 || tiles[0].Width != 800 {
		t.Fatalf("tiles = %+v", tiles)
	}
}

func TestTilingPlanLongImageOverlapAndOrder(t *testing.T) {
	const height = 3000
	tiles, err := TilingPlan(750, height)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(tiles) < 3 {
		t.Fatalf("expected >=3 tiles, got %d", len(tiles))
	}
	for i, tile := range tiles {
		if tile.Index != i {
			t.Fatalf("reading order broken at %d: %+v", i, tile)
		}
		if i > 0 {
			prev := tiles[i-1]
			overlap := prev.Y + prev.Height - tile.Y
			if overlap != tileOverlap {
				t.Fatalf("tile %d overlap = %d, want %d", i, overlap, tileOverlap)
			}
		}
	}
	last := tiles[len(tiles)-1]
	if last.Y+last.Height != height {
		t.Fatalf("tiles do not cover full height: last=%+v", last)
	}
}

func TestTilingPlanRejectsInvalidSize(t *testing.T) {
	if _, err := TilingPlan(0, 100); err == nil {
		t.Fatal("zero width must fail")
	}
}
