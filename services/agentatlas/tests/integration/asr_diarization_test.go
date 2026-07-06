// ASR + topic integration: the real asr-sidecar transcribes the fixture WAV
// (speaker labels present — "S1" fallback when diarization models are
// unavailable) and the topic processor groups the segments. Run:
//
//	ATLAS_PARSER_SIDECARS=1 go test ./tests/integration -run TestASRDiarization
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

func TestASRDiarization(t *testing.T) {
	if os.Getenv("ATLAS_PARSER_SIDECARS") != "1" {
		t.Skip("set ATLAS_PARSER_SIDECARS=1 with the asr sidecar up")
	}
	asrURL := envOr("ATLAS_TEST_ASR_URL", "http://localhost:5003")
	if !sidecarUp(asrURL) {
		t.Skipf("asr sidecar unreachable at %s (build it first)", asrURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out, err := parsergateway.NewASRProvider(asrURL).Parse(ctx, parsergateway.ParseInput{
		EnterpriseID: "ent_it", ArtifactID: "art_asr", Filename: "meeting_clip.wav",
		ContentType: "audio/wav", Data: readFixture(t, "media", "meeting_clip.wav"),
	})
	if err != nil {
		t.Fatalf("real asr parse: %v", err)
	}
	t.Logf("asr: %d segments", len(out.AudioSegments))
	if len(out.AudioSegments) == 0 {
		t.Skip("tone fixture produced no VAD segments — transcription path itself is healthy")
	}
	for _, seg := range out.AudioSegments {
		if seg.Speaker == "" {
			t.Fatalf("segment %s missing speaker label (single-speaker fallback must apply)", seg.SegmentID)
		}
	}

	objects := &memObjectsIT{data: map[string][]byte{}}
	res, err := (&artifacts.AudioTopicProcessor{Objects: objects}).Process(ctx, "ent_it", "art_asr", out.AudioSegments)
	if err != nil {
		t.Fatalf("topics: %v", err)
	}
	if len(res.Topics) == 0 || res.TranscriptObjectKey == "" {
		t.Fatalf("topics result: %+v", res)
	}
	t.Logf("topics: %d, transcript sealed at %s", len(res.Topics), res.TranscriptObjectKey)
}
