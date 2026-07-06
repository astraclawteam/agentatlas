package parsergateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
)

// ASRProvider talks to the asr-sidecar (sidecars/asr-sidecar, POST /parse).
type ASRProvider struct {
	baseURL string
	http    *http.Client
}

func NewASRProvider(baseURL string) *ASRProvider {
	return &ASRProvider{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Minute}}
}

func (p *ASRProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID:      "asr",
		Capabilities:    []string{sdkparser.CapAudioASR},
		InputTypes:      []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/x-m4a"},
		OutputSchema:    sdkparser.OutputSchemaV1,
		MaxFileSizeMB:   500,
		SupportsPrivate: true,
	}
}

type asrResponse struct {
	Language      string `json:"language"`
	DurationMS    int    `json:"duration_ms"`
	Diarization   string `json:"diarization"`
	AudioSegments []struct {
		SegmentID string `json:"segment_id"`
		StartMS   int    `json:"start_ms"`
		EndMS     int    `json:"end_ms"`
		Speaker   string `json:"speaker"`
		Text      string `json:"text"`
	} `json:"audio_segments"`
}

func (p *ASRProvider) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	var resp asrResponse
	err := postMultipart(ctx, p.http, p.baseURL+"/parse",
		map[string]string{"artifact_id": in.ArtifactID},
		"file", nonEmpty(in.Filename, in.ArtifactID+".wav"), in.Data, &resp)
	if err != nil {
		return ParseOutput{}, err
	}
	segments := make([]atlasdocument.AudioSegment, 0, len(resp.AudioSegments))
	for _, s := range resp.AudioSegments {
		segments = append(segments, atlasdocument.AudioSegment{
			SegmentID: s.SegmentID, StartMS: s.StartMS, EndMS: s.EndMS,
			Speaker: s.Speaker, Text: s.Text,
		})
	}
	return ParseOutput{AudioSegments: segments, Confidence: 0.85}, nil
}
