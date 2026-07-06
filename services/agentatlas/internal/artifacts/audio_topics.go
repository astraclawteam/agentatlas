package artifacts

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
)

// topicGapMS starts a new topic when the silence between segments exceeds it.
const topicGapMS = 2000

// TopicSummarizer condenses one topic's segment texts (production: an
// llmutil-backed closure over the model route; nil = deterministic fallback).
type TopicSummarizer func(ctx context.Context, texts []string) (string, error)

// AudioTopic is one diarization/topic-aware slice of the recording. Only the
// sanitized summary lives in metadata; the full transcript is sealed.
type AudioTopic struct {
	Index      int      `json:"index"`
	StartMS    int      `json:"start_ms"`
	EndMS      int      `json:"end_ms"`
	Speakers   []string `json:"speakers,omitempty"`
	Summary    string   `json:"summary"`
	SegmentIDs []string `json:"segment_ids"`
}

// AudioTopicsResult carries the topics plus the sealed transcript pointer.
type AudioTopicsResult struct {
	Topics              []AudioTopic `json:"topics"`
	TranscriptObjectKey string       `json:"transcript_object_key"`
	Source              string       `json:"source"` // "llm" | "deterministic"
}

// AudioTopicProcessor groups ASR segments into topics (speaker-change or
// silence-gap boundaries), summarizes each topic, and seals the full
// transcript to object storage.
type AudioTopicProcessor struct {
	Summarize TopicSummarizer
	Objects   Objects
}

// Process segments -> topics for (enterpriseID, artifactID).
func (p *AudioTopicProcessor) Process(ctx context.Context, enterpriseID, artifactID string, segments []atlasdocument.AudioSegment) (AudioTopicsResult, error) {
	if p.Objects == nil {
		return AudioTopicsResult{}, fmt.Errorf("audio topics: object storage required (full transcript is sealed, never in metadata)")
	}
	if len(segments) == 0 {
		return AudioTopicsResult{}, fmt.Errorf("audio topics: no segments to group")
	}
	ordered := make([]atlasdocument.AudioSegment, len(segments))
	copy(ordered, segments)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartMS < ordered[j].StartMS })

	// Boundary rule: speaker change (when diarized) or a silence gap.
	var groups [][]atlasdocument.AudioSegment
	cur := []atlasdocument.AudioSegment{ordered[0]}
	for _, seg := range ordered[1:] {
		prev := cur[len(cur)-1]
		speakerChanged := seg.Speaker != "" && prev.Speaker != "" && seg.Speaker != prev.Speaker
		gapped := seg.StartMS-prev.EndMS > topicGapMS
		if speakerChanged || gapped {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, seg)
	}
	groups = append(groups, cur)

	// Seal the full transcript before building sanitized topic summaries.
	var transcript strings.Builder
	for _, seg := range ordered {
		if seg.Speaker != "" {
			fmt.Fprintf(&transcript, "[%s %d-%dms] %s\n", seg.Speaker, seg.StartMS, seg.EndMS, seg.Text)
		} else {
			fmt.Fprintf(&transcript, "[%d-%dms] %s\n", seg.StartMS, seg.EndMS, seg.Text)
		}
	}
	transcriptKey := fmt.Sprintf("audio/%s/%s/transcript.txt", enterpriseID, artifactID)
	if err := p.Objects.Put(ctx, transcriptKey, "text/plain", []byte(transcript.String())); err != nil {
		return AudioTopicsResult{}, fmt.Errorf("seal transcript: %w", err)
	}

	source := "deterministic"
	topics := make([]AudioTopic, 0, len(groups))
	for i, group := range groups {
		texts := make([]string, 0, len(group))
		ids := make([]string, 0, len(group))
		speakerSet := map[string]bool{}
		for _, seg := range group {
			if seg.Text != "" {
				texts = append(texts, seg.Text)
			}
			ids = append(ids, seg.SegmentID)
			if seg.Speaker != "" {
				speakerSet[seg.Speaker] = true
			}
		}
		summary := truncateRunes(strings.Join(texts, " "), 200) // deterministic fallback
		if p.Summarize != nil && len(texts) > 0 {
			out, err := p.Summarize(ctx, texts)
			if err != nil {
				return AudioTopicsResult{}, fmt.Errorf("topic %d summary: %w", i, err)
			}
			summary = truncateRunes(strings.TrimSpace(out), 200)
			source = "llm"
		}
		speakers := make([]string, 0, len(speakerSet))
		for s := range speakerSet {
			speakers = append(speakers, s)
		}
		sort.Strings(speakers)
		topics = append(topics, AudioTopic{
			Index: i, StartMS: group[0].StartMS, EndMS: group[len(group)-1].EndMS,
			Speakers: speakers, Summary: summary, SegmentIDs: ids,
		})
	}
	return AudioTopicsResult{Topics: topics, TranscriptObjectKey: transcriptKey, Source: source}, nil
}
