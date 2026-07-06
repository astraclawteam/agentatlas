package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
)

// TranscribeFn runs ASR over the extracted audio track (production: the ASR
// provider against the sidecar's audio_shared_path re-keyed through storage).
type TranscribeFn func(ctx context.Context, audioRef string) ([]atlasdocument.AudioSegment, error)

// KeyframeLoader fetches one keyframe image (shared-volume path or object key).
type KeyframeLoader func(ctx context.Context, ref string) ([]byte, error)

// SceneAlignment is one scene with its aligned transcript + keyframe
// understanding. Only capped summaries/excerpts live here; full artifacts are
// sealed to object storage.
type SceneAlignment struct {
	SceneIndex         int      `json:"scene_index"`
	StartMS            int      `json:"start_ms"`
	EndMS              int      `json:"end_ms"`
	Summary            string   `json:"summary"`
	TranscriptExcerpt  string   `json:"transcript_excerpt,omitempty"`
	KeyframeDesc       string   `json:"keyframe_desc,omitempty"`
	AudioSegmentIDs    []string `json:"audio_segment_ids,omitempty"`
	VideoSegmentID     string   `json:"video_segment_id"`
	KeyframeObjectKey  string   `json:"keyframe_object_key,omitempty"`
}

// VideoAlignResult carries the aligned scenes plus sealed-object pointers.
type VideoAlignResult struct {
	Scenes                 []SceneAlignment `json:"scenes"`
	TranscriptObjectKey    string           `json:"transcript_object_key,omitempty"`
	KeyframeNotesObjectKey string           `json:"keyframe_notes_object_key,omitempty"`
	Source                 string           `json:"source"` // "transcript+vlm" | "transcript_only" | "keyframes_only"
}

// VideoAlignProcessor aligns the video sidecar's scenes/keyframes with the
// ASR transcript and optional keyframe OCR/VLM into per-scene summaries.
// Nil VLM = explicit transcript-only degraded mode.
type VideoAlignProcessor struct {
	Transcribe TranscribeFn
	Keyframes  KeyframeLoader
	OCR        TileOCR         // optional keyframe OCR
	VLM        RegionDescriber // optional keyframe description
	Sum        TopicSummarizer // optional per-scene summary
	Objects    Objects
}

// Process aligns one parsed video for (enterpriseID, artifactID).
func (p *VideoAlignProcessor) Process(ctx context.Context, enterpriseID, artifactID, audioRef string, scenes []atlasdocument.VideoSegment) (VideoAlignResult, error) {
	if p.Objects == nil {
		return VideoAlignResult{}, fmt.Errorf("video align: object storage required (full transcript/keyframe notes are sealed)")
	}
	if len(scenes) == 0 {
		return VideoAlignResult{}, fmt.Errorf("video align: no scenes from the video sidecar")
	}
	ordered := make([]atlasdocument.VideoSegment, len(scenes))
	copy(ordered, scenes)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartMS < ordered[j].StartMS })

	// Transcript leg (optional when no ASR is wired).
	var transcript []atlasdocument.AudioSegment
	if p.Transcribe != nil && audioRef != "" {
		var err error
		transcript, err = p.Transcribe(ctx, audioRef)
		if err != nil {
			return VideoAlignResult{}, fmt.Errorf("video align transcribe: %w", err)
		}
	}
	if transcript == nil && p.VLM == nil && p.OCR == nil {
		return VideoAlignResult{}, fmt.Errorf("video align: neither transcript nor keyframe understanding configured")
	}

	result := VideoAlignResult{Source: "keyframes_only"}
	if len(transcript) > 0 {
		result.Source = "transcript_only"
		var full strings.Builder
		for _, seg := range transcript {
			fmt.Fprintf(&full, "[%d-%dms] %s\n", seg.StartMS, seg.EndMS, seg.Text)
		}
		result.TranscriptObjectKey = fmt.Sprintf("video/%s/%s/transcript.txt", enterpriseID, artifactID)
		if err := p.Objects.Put(ctx, result.TranscriptObjectKey, "text/plain", []byte(full.String())); err != nil {
			return VideoAlignResult{}, fmt.Errorf("seal transcript: %w", err)
		}
	}

	keyframeNotes := map[string]string{} // full descriptions, sealed
	for _, scene := range ordered {
		align := SceneAlignment{
			SceneIndex: scene.SceneIndex, StartMS: scene.StartMS, EndMS: scene.EndMS,
			VideoSegmentID: scene.SegmentID, KeyframeObjectKey: scene.KeyframeObjectKey,
		}

		// transcript ↔ scene by timestamp overlap
		var texts []string
		for _, seg := range transcript {
			if seg.StartMS < scene.EndMS && seg.EndMS > scene.StartMS {
				if seg.Text != "" {
					texts = append(texts, seg.Text)
				}
				align.AudioSegmentIDs = append(align.AudioSegmentIDs, seg.SegmentID)
			}
		}
		align.TranscriptExcerpt = truncateRunes(strings.Join(texts, " "), maxExcerptLen)

		// keyframe ↔ OCR/VLM (optional legs)
		if (p.VLM != nil || p.OCR != nil) && p.Keyframes != nil && scene.KeyframeObjectKey != "" {
			frame, err := p.Keyframes(ctx, scene.KeyframeObjectKey)
			if err != nil {
				return VideoAlignResult{}, fmt.Errorf("scene %d keyframe: %w", scene.SceneIndex, err)
			}
			var notes []string
			if p.OCR != nil {
				text, err := p.OCR.OCRTile(ctx, frame, "image/png")
				if err != nil {
					return VideoAlignResult{}, fmt.Errorf("scene %d keyframe ocr: %w", scene.SceneIndex, err)
				}
				if strings.TrimSpace(text) != "" {
					notes = append(notes, "OCR: "+text)
				}
			}
			if p.VLM != nil {
				desc, err := p.VLM(ctx, frame, "image/png",
					"这是一个视频场景的关键帧。请用中文简要描述画面内容（不超过60字），不得虚构。")
				if err != nil {
					return VideoAlignResult{}, fmt.Errorf("scene %d keyframe vlm: %w", scene.SceneIndex, err)
				}
				notes = append(notes, strings.TrimSpace(desc))
				result.Source = "transcript+vlm"
			}
			fullNote := strings.Join(notes, "\n")
			keyframeNotes[scene.SegmentID] = fullNote
			align.KeyframeDesc = truncateRunes(fullNote, 200)
		}

		// per-scene summary: transcript + keyframe notes
		parts := texts
		if align.KeyframeDesc != "" {
			parts = append(parts, align.KeyframeDesc)
		}
		summary := truncateRunes(strings.Join(parts, " "), 200)
		if p.Sum != nil && len(parts) > 0 {
			out, err := p.Sum(ctx, parts)
			if err != nil {
				return VideoAlignResult{}, fmt.Errorf("scene %d summary: %w", scene.SceneIndex, err)
			}
			summary = truncateRunes(strings.TrimSpace(out), 200)
		}
		align.Summary = summary
		result.Scenes = append(result.Scenes, align)
	}

	if len(keyframeNotes) > 0 {
		raw, err := json.Marshal(keyframeNotes)
		if err != nil {
			return VideoAlignResult{}, err
		}
		result.KeyframeNotesObjectKey = fmt.Sprintf("video/%s/%s/keyframe_notes.json", enterpriseID, artifactID)
		if err := p.Objects.Put(ctx, result.KeyframeNotesObjectKey, "application/json", raw); err != nil {
			return VideoAlignResult{}, fmt.Errorf("seal keyframe notes: %w", err)
		}
	}
	return result, nil
}
