package parsergateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
)

// VideoProvider talks to the video-sidecar (sidecars/video-sidecar,
// POST /parse): ffmpeg audio extraction + PySceneDetect scenes + keyframes.
// Keyframe/audio files land on the shared volume; the artifact service moves
// them into object storage.
type VideoProvider struct {
	baseURL string
	http    *http.Client
}

func NewVideoProvider(baseURL string) *VideoProvider {
	return &VideoProvider{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 60 * time.Minute}}
}

func (p *VideoProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID:      "video",
		Capabilities:    []string{sdkparser.CapVideoScene},
		InputTypes:      []string{"video/mp4", "video/quicktime", "video/x-matroska"},
		OutputSchema:    sdkparser.OutputSchemaV1,
		MaxFileSizeMB:   2048,
		SupportsPrivate: true,
	}
}

type videoResponse struct {
	JobID           string `json:"job_id"`
	AudioSharedPath string `json:"audio_shared_path"`
	VideoSegments   []struct {
		SegmentID         string `json:"segment_id"`
		SceneIndex        int    `json:"scene_index"`
		StartMS           int    `json:"start_ms"`
		EndMS             int    `json:"end_ms"`
		KeyframeSharedPath string `json:"keyframe_shared_path"`
	} `json:"video_segments"`
}

func (p *VideoProvider) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	var resp videoResponse
	err := postMultipart(ctx, p.http, p.baseURL+"/parse",
		map[string]string{"artifact_id": in.ArtifactID},
		"file", nonEmpty(in.Filename, in.ArtifactID+".mp4"), in.Data, &resp)
	if err != nil {
		return ParseOutput{}, err
	}
	segments := make([]atlasdocument.VideoSegment, 0, len(resp.VideoSegments))
	for _, s := range resp.VideoSegments {
		segments = append(segments, atlasdocument.VideoSegment{
			SegmentID:         s.SegmentID,
			SceneIndex:        s.SceneIndex,
			StartMS:           s.StartMS,
			EndMS:             s.EndMS,
			KeyframeObjectKey: s.KeyframeSharedPath, // re-keyed into object storage by the artifact service
		})
	}
	return ParseOutput{VideoSegments: segments, Confidence: 0.8}, nil
}
