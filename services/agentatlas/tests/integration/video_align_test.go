// Video alignment integration: real video sidecar scenes + real ASR transcript
// aligned into per-scene summaries. Run:
//
//	ATLAS_PARSER_SIDECARS=1 go test ./tests/integration -run TestVideoAlign
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

func TestVideoAlign(t *testing.T) {
	if os.Getenv("ATLAS_PARSER_SIDECARS") != "1" {
		t.Skip("set ATLAS_PARSER_SIDECARS=1 with the video (and asr) sidecars up")
	}
	videoURL := envOr("ATLAS_TEST_VIDEO_URL", "http://localhost:5004")
	if !sidecarUp(videoURL) {
		t.Skipf("video sidecar unreachable at %s (build it first)", videoURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	out, err := parsergateway.NewVideoProvider(videoURL).Parse(ctx, parsergateway.ParseInput{
		EnterpriseID: "ent_it", ArtifactID: "art_video", Filename: "line_walk.mp4",
		ContentType: "video/mp4", Data: readFixture(t, "media", "line_walk.mp4"),
	})
	if err != nil {
		t.Fatalf("real video parse: %v", err)
	}
	if len(out.VideoSegments) == 0 {
		t.Fatalf("no scenes: %+v", out)
	}

	// Alignment with a deterministic transcript stand-in when the ASR sidecar
	// is absent; the real ASR path is covered by TestASRDiarization.
	transcribe := func(context.Context, string) ([]atlasdocument.AudioSegment, error) {
		return []atlasdocument.AudioSegment{
			{SegmentID: "a1", StartMS: 0, EndMS: 1000, Text: "巡线开始。"},
			{SegmentID: "a2", StartMS: 1000, EndMS: 2000, Text: "巡线结束。"},
		}, nil
	}
	asrURL := envOr("ATLAS_TEST_ASR_URL", "http://localhost:5003")
	if sidecarUp(asrURL) {
		provider := parsergateway.NewASRProvider(asrURL)
		transcribe = func(ctx context.Context, _ string) ([]atlasdocument.AudioSegment, error) {
			po, err := provider.Parse(ctx, parsergateway.ParseInput{
				EnterpriseID: "ent_it", ArtifactID: "art_video_audio", Filename: "meeting_clip.wav",
				ContentType: "audio/wav", Data: readFixture(t, "media", "meeting_clip.wav"),
			})
			if err != nil {
				return nil, err
			}
			return po.AudioSegments, nil
		}
	}

	res, err := (&artifacts.VideoAlignProcessor{
		Transcribe: transcribe,
		Objects:    &memObjectsIT{data: map[string][]byte{}},
	}).Process(ctx, "ent_it", "art_video", "shared/audio.wav", out.VideoSegments)
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if len(res.Scenes) == 0 {
		t.Fatal("no aligned scenes")
	}
	t.Logf("video align: %d scenes source=%s first summary=%q", len(res.Scenes), res.Source, res.Scenes[0].Summary)
}
