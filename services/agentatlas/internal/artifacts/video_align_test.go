package artifacts

import (
	"context"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
)

func videoScenes() []atlasdocument.VideoSegment {
	return []atlasdocument.VideoSegment{
		{SegmentID: "v1", SceneIndex: 0, StartMS: 0, EndMS: 5000, KeyframeObjectKey: "kf/v1.png"},
		{SegmentID: "v2", SceneIndex: 1, StartMS: 5000, EndMS: 12000, KeyframeObjectKey: "kf/v2.png"},
	}
}

func videoTranscript() []atlasdocument.AudioSegment {
	return []atlasdocument.AudioSegment{
		{SegmentID: "a1", StartMS: 500, EndMS: 4500, Text: "开场介绍产线巡检的目标。"},
		{SegmentID: "a2", StartMS: 6000, EndMS: 11000, Text: "第二段讲解异常工单的处理流程。"},
	}
}

func TestVideoAlignTranscriptSceneKeyframe(t *testing.T) {
	objects := &memObjectsLI{data: map[string][]byte{}}
	p := &VideoAlignProcessor{
		Transcribe: func(context.Context, string) ([]atlasdocument.AudioSegment, error) {
			return videoTranscript(), nil
		},
		Keyframes: func(_ context.Context, ref string) ([]byte, error) { return []byte("img:" + ref), nil },
		VLM: func(_ context.Context, frame []byte, _, _ string) (string, error) {
			return "画面：产线设备特写（" + string(frame[:6]) + "）", nil
		},
		Objects: objects,
	}
	res, err := p.Process(context.Background(), "ent_1", "art_v", "audio/shared.wav", videoScenes())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(res.Scenes) != 2 {
		t.Fatalf("scenes = %d", len(res.Scenes))
	}
	// timestamp alignment: a1 -> scene0, a2 -> scene1
	if got := res.Scenes[0].AudioSegmentIDs; len(got) != 1 || got[0] != "a1" {
		t.Fatalf("scene0 audio = %v", got)
	}
	if got := res.Scenes[1].AudioSegmentIDs; len(got) != 1 || got[0] != "a2" {
		t.Fatalf("scene1 audio = %v", got)
	}
	if !strings.Contains(res.Scenes[0].TranscriptExcerpt, "巡检") {
		t.Fatalf("scene0 excerpt = %q", res.Scenes[0].TranscriptExcerpt)
	}
	if res.Scenes[0].KeyframeDesc == "" || res.Source != "transcript+vlm" {
		t.Fatalf("keyframe leg missing: %+v source=%s", res.Scenes[0], res.Source)
	}
	// sealed artifacts exist; metadata capped
	if _, ok := objects.data[res.TranscriptObjectKey]; !ok {
		t.Fatal("transcript not sealed")
	}
	if _, ok := objects.data[res.KeyframeNotesObjectKey]; !ok {
		t.Fatal("keyframe notes not sealed")
	}
	for _, s := range res.Scenes {
		if len([]rune(s.Summary)) > 200 || len([]rune(s.TranscriptExcerpt)) > maxExcerptLen {
			t.Fatalf("caps violated: %+v", s)
		}
	}
}

func TestVideoAlignTranscriptOnlyFallback(t *testing.T) {
	p := &VideoAlignProcessor{
		Transcribe: func(context.Context, string) ([]atlasdocument.AudioSegment, error) {
			return videoTranscript(), nil
		},
		Objects: &memObjectsLI{data: map[string][]byte{}},
	}
	res, err := p.Process(context.Background(), "ent_1", "art_v2", "audio.wav", videoScenes())
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "transcript_only" {
		t.Fatalf("source = %q", res.Source)
	}
	for _, s := range res.Scenes {
		if s.KeyframeDesc != "" {
			t.Fatal("no VLM configured but keyframe description present")
		}
	}
}

func TestVideoAlignFailLoud(t *testing.T) {
	if _, err := (&VideoAlignProcessor{}).Process(context.Background(), "e", "a", "", videoScenes()); err == nil {
		t.Fatal("missing object storage must fail loud")
	}
	p := &VideoAlignProcessor{Objects: &memObjectsLI{data: map[string][]byte{}}}
	if _, err := p.Process(context.Background(), "e", "a", "", videoScenes()); err == nil {
		t.Fatal("no transcript and no keyframe understanding must fail loud")
	}
	if _, err := p.Process(context.Background(), "e", "a", "", nil); err == nil {
		t.Fatal("zero scenes must fail loud")
	}
}
