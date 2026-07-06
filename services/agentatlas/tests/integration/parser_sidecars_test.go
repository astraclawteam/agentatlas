// Real parser-sidecar integration: exercises the REAL Docling/MinerU/ASR/video
// sidecars from deploy/compose against real fixture media through the actual
// provider clients. Gated by ATLAS_PARSER_SIDECARS=1; each sidecar leg skips
// loudly when its endpoint is unreachable (e.g. image not built yet) and fails
// when reachable but broken.
//
//	docker compose -f deploy/compose/compose.yaml up -d docling-sidecar mineru-sidecar asr-sidecar video-sidecar
//	ATLAS_PARSER_SIDECARS=1 go test ./tests/integration -run TestParserSidecars -v
package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

func sidecarUp(url string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", "fixtures"}, parts...)...))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return raw
}

func TestParserSidecars(t *testing.T) {
	if os.Getenv("ATLAS_PARSER_SIDECARS") != "1" {
		t.Skip("set ATLAS_PARSER_SIDECARS=1 with the compose sidecars up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // first ASR run downloads models
	defer cancel()

	doclingURL := envOr("ATLAS_TEST_DOCLING_URL", "http://localhost:5001")
	mineruURL := envOr("ATLAS_TEST_MINERU_URL", "http://localhost:5002")
	asrURL := envOr("ATLAS_TEST_ASR_URL", "http://localhost:5003")
	videoURL := envOr("ATLAS_TEST_VIDEO_URL", "http://localhost:5004")

	t.Run("docling_pdf", func(t *testing.T) {
		if !sidecarUp(doclingURL) {
			t.Skipf("docling sidecar unreachable at %s (compose service down)", doclingURL)
		}
		out, err := parsergateway.NewDoclingProvider(doclingURL).Parse(ctx, parsergateway.ParseInput{
			EnterpriseID: "ent_it", ArtifactID: "art_pdf", Filename: "mes_work_order.pdf",
			ContentType: "application/pdf", Data: readFixture(t, "documents", "mes_work_order.pdf"),
		})
		if err != nil {
			t.Fatalf("real docling parse: %v", err)
		}
		if len(out.Blocks) == 0 {
			t.Fatalf("real docling returned no blocks: %+v", out)
		}
		if out.Confidence <= 0 {
			t.Fatalf("confidence not set: %v", out.Confidence)
		}
		t.Logf("docling: %d blocks, confidence %.2f, first: %q", len(out.Blocks), out.Confidence, out.Blocks[0].Text)
	})

	t.Run("mineru_pdf", func(t *testing.T) {
		if !sidecarUp(mineruURL) {
			t.Skipf("mineru sidecar unreachable at %s (build it from PyPI first)", mineruURL)
		}
		out, err := parsergateway.NewMinerUProvider(mineruURL).Parse(ctx, parsergateway.ParseInput{
			EnterpriseID: "ent_it", ArtifactID: "art_pdf2", Filename: "mes_work_order.pdf",
			ContentType: "application/pdf", Data: readFixture(t, "documents", "mes_work_order.pdf"),
		})
		if err != nil {
			t.Fatalf("real mineru parse: %v", err)
		}
		if len(out.Blocks) == 0 && len(out.Tables) == 0 {
			t.Fatalf("real mineru returned no structure: %+v", out)
		}
		t.Logf("mineru: %d blocks, confidence %.2f", len(out.Blocks), out.Confidence)
	})

	t.Run("asr_wav", func(t *testing.T) {
		if !sidecarUp(asrURL) {
			t.Skipf("asr sidecar unreachable at %s (build it first)", asrURL)
		}
		out, err := parsergateway.NewASRProvider(asrURL).Parse(ctx, parsergateway.ParseInput{
			EnterpriseID: "ent_it", ArtifactID: "art_wav", Filename: "meeting_clip.wav",
			ContentType: "audio/wav", Data: readFixture(t, "media", "meeting_clip.wav"),
		})
		if err != nil {
			t.Fatalf("real asr parse: %v", err)
		}
		// The fixture is synthesized tone — transcript text may be empty, but
		// the segment structure must come back well-formed.
		if out.Confidence <= 0 {
			t.Fatalf("asr confidence not set: %+v", out)
		}
		t.Logf("asr: %d segments, confidence %.2f", len(out.AudioSegments), out.Confidence)
	})

	t.Run("video_mp4", func(t *testing.T) {
		if !sidecarUp(videoURL) {
			t.Skipf("video sidecar unreachable at %s (build it first)", videoURL)
		}
		out, err := parsergateway.NewVideoProvider(videoURL).Parse(ctx, parsergateway.ParseInput{
			EnterpriseID: "ent_it", ArtifactID: "art_mp4", Filename: "line_walk.mp4",
			ContentType: "video/mp4", Data: readFixture(t, "media", "line_walk.mp4"),
		})
		if err != nil {
			t.Fatalf("real video parse: %v", err)
		}
		if len(out.VideoSegments) == 0 {
			t.Fatalf("real video returned no segments: %+v", out)
		}
		t.Logf("video: %d segments, confidence %.2f", len(out.VideoSegments), out.Confidence)
	})
}
