package artifacts

import (
	"context"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
)

func meetingSegments() []atlasdocument.AudioSegment {
	return []atlasdocument.AudioSegment{
		{SegmentID: "s1", StartMS: 0, EndMS: 4000, Speaker: "S1", Text: "今天同步 MES 工单排查进展，分拣规则联调已经完成。"},
		{SegmentID: "s2", StartMS: 4200, EndMS: 8000, Speaker: "S1", Text: "版本复核也过了，遗留一个接口限流问题。"},
		{SegmentID: "s3", StartMS: 8100, EndMS: 12000, Speaker: "S2", Text: "限流方案我建议先按队列削峰做一版。"},
		{SegmentID: "s4", StartMS: 16000, EndMS: 20000, Speaker: "S2", Text: "另外下周的部署窗口需要跟运维确认。"},
	}
}

func TestAudioTopicsSpeakerAndGapBoundaries(t *testing.T) {
	objects := &memObjectsLI{data: map[string][]byte{}}
	p := &AudioTopicProcessor{Objects: objects}
	res, err := p.Process(context.Background(), "ent_1", "art_wav", meetingSegments())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	// boundaries: S1->S2 speaker change at s3, 4s gap before s4 => 3 topics
	if len(res.Topics) != 3 {
		t.Fatalf("topics = %d, want 3: %+v", len(res.Topics), res.Topics)
	}
	if got := res.Topics[0].Speakers; len(got) != 1 || got[0] != "S1" {
		t.Fatalf("topic0 speakers = %v", got)
	}
	if got := res.Topics[1].Speakers; len(got) != 1 || got[0] != "S2" {
		t.Fatalf("topic1 speakers = %v", got)
	}
	if res.Source != "deterministic" {
		t.Fatalf("source = %q", res.Source)
	}
	// full transcript sealed with speaker labels; summaries capped
	sealed := string(objects.data[res.TranscriptObjectKey])
	if !strings.Contains(sealed, "[S1 0-4000ms]") || !strings.Contains(sealed, "[S2 16000-20000ms]") {
		t.Fatalf("transcript not sealed with labels: %s", sealed)
	}
	for _, topic := range res.Topics {
		if n := len([]rune(topic.Summary)); n > 200 {
			t.Fatalf("topic summary exceeds cap: %d", n)
		}
	}
}

func TestAudioTopicsSingleSpeakerFallback(t *testing.T) {
	// no speaker labels (diarization unavailable) -> gap-only segmentation
	segs := []atlasdocument.AudioSegment{
		{SegmentID: "a", StartMS: 0, EndMS: 3000, Text: "第一段"},
		{SegmentID: "b", StartMS: 3100, EndMS: 6000, Text: "还是第一段"},
		{SegmentID: "c", StartMS: 12000, EndMS: 15000, Text: "第二段"},
	}
	p := &AudioTopicProcessor{Objects: &memObjectsLI{data: map[string][]byte{}}}
	res, err := p.Process(context.Background(), "ent_1", "art_w2", segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Topics) != 2 {
		t.Fatalf("topics = %d, want 2 (gap only)", len(res.Topics))
	}
}

func TestAudioTopicsLLMAssisted(t *testing.T) {
	p := &AudioTopicProcessor{
		Objects: &memObjectsLI{data: map[string][]byte{}},
		Summarize: func(_ context.Context, texts []string) (string, error) {
			return "模型主题摘要（" + strings.Join(texts, "|")[:8] + "…）", nil
		},
	}
	res, err := p.Process(context.Background(), "ent_1", "art_w3", meetingSegments())
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "llm" {
		t.Fatalf("source = %q", res.Source)
	}
	for _, topic := range res.Topics {
		if !strings.Contains(topic.Summary, "模型主题摘要") {
			t.Fatalf("topic summary not model-derived: %q", topic.Summary)
		}
	}
}

func TestAudioTopicsFailLoud(t *testing.T) {
	p := &AudioTopicProcessor{}
	if _, err := p.Process(context.Background(), "e", "a", meetingSegments()); err == nil {
		t.Fatal("missing object storage must fail loud")
	}
	p2 := &AudioTopicProcessor{Objects: &memObjectsLI{data: map[string][]byte{}}}
	if _, err := p2.Process(context.Background(), "e", "a", nil); err == nil {
		t.Fatal("zero segments must fail loud")
	}
}
