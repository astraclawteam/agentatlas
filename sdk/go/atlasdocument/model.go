// Package atlasdocument mirrors schemas/atlasdocument/atlas-document.schema.json.
// The full AtlasDocument lives in object storage; AgentAtlas metadata tables
// keep only hashes, sanitized summaries, and evidence pointers.
package atlasdocument

type BlockType string

const (
	BlockText    BlockType = "text"
	BlockHeading BlockType = "heading"
	BlockTable   BlockType = "table"
	BlockFigure  BlockType = "figure"
	BlockCaption BlockType = "caption"
	BlockList    BlockType = "list"
	BlockCode    BlockType = "code"
)

type Block struct {
	BlockID string    `json:"block_id"`
	Type    BlockType `json:"type"`
	Order   int       `json:"order"`
	Page    int       `json:"page,omitempty"`
	Text    string    `json:"text,omitempty"`
	BBox    []float64 `json:"bbox,omitempty"`
}

type TableCell struct {
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	Text    string `json:"text"`
	RowSpan int    `json:"rowspan,omitempty"`
	ColSpan int    `json:"colspan,omitempty"`
}

type Table struct {
	TableID string      `json:"table_id"`
	Page    int         `json:"page,omitempty"`
	NRows   int         `json:"n_rows,omitempty"`
	NCols   int         `json:"n_cols,omitempty"`
	Cells   []TableCell `json:"cells,omitempty"`
	Summary string      `json:"summary,omitempty"`
}

type ImageRegion struct {
	ImageID    string    `json:"image_id"`
	Page       int       `json:"page,omitempty"`
	BBox       []float64 `json:"bbox,omitempty"`
	ObjectKey  string    `json:"object_key,omitempty"`
	OCRText    string    `json:"ocr_text,omitempty"`
	VLMCaption string    `json:"vlm_caption,omitempty"`
}

type AudioSegment struct {
	SegmentID string `json:"segment_id"`
	StartMS   int    `json:"start_ms"`
	EndMS     int    `json:"end_ms"`
	Speaker   string `json:"speaker,omitempty"`
	Text      string `json:"text,omitempty"`
}

type VideoSegment struct {
	SegmentID         string `json:"segment_id"`
	StartMS           int    `json:"start_ms"`
	EndMS             int    `json:"end_ms"`
	SceneIndex        int    `json:"scene_index,omitempty"`
	KeyframeObjectKey string `json:"keyframe_object_key,omitempty"`
	Transcript        string `json:"transcript,omitempty"`
	KeyframeCaption   string `json:"keyframe_caption,omitempty"`
}

type SummaryLevel string

const (
	SummarySection   SummaryLevel = "section"
	SummaryDocument  SummaryLevel = "document"
	SummaryDisplay   SummaryLevel = "display"
	SummaryRetrieval SummaryLevel = "retrieval"
)

type Summary struct {
	SummaryID string       `json:"summary_id"`
	Level     SummaryLevel `json:"level"`
	Ref       string       `json:"ref,omitempty"`
	Text      string       `json:"text"`
}

// EvidencePointer references a resource locatable through AgentNexus. It never
// contains original content.
type EvidencePointer struct {
	EvidenceID            string   `json:"evidence_id,omitempty"`
	EnterpriseID          string   `json:"enterprise_id"`
	ResourceType          string   `json:"resource_type"`
	ResourceRef           string   `json:"resource_ref"`
	SourceSystem          string   `json:"source_system"`
	ContentHash           string   `json:"content_hash,omitempty"`
	SummaryHash           string   `json:"summary_hash,omitempty"`
	AgentNexusResourceURI string   `json:"agentnexus_resource_uri,omitempty"`
	RequiredScopes        []string `json:"required_scopes,omitempty"`
	CreatedAt             string   `json:"created_at,omitempty"`
}

type AtlasDocument struct {
	AtlasDocumentID  string            `json:"atlas_document_id"`
	ArtifactID       string            `json:"artifact_id"`
	SourceHash       string            `json:"source_hash"`
	ContentType      string            `json:"content_type"`
	Provider         string            `json:"provider,omitempty"`
	Confidence       float64           `json:"confidence,omitempty"`
	Blocks           []Block           `json:"blocks,omitempty"`
	Tables           []Table           `json:"tables,omitempty"`
	Images           []ImageRegion     `json:"images,omitempty"`
	AudioSegments    []AudioSegment    `json:"audio_segments,omitempty"`
	VideoSegments    []VideoSegment    `json:"video_segments,omitempty"`
	Summaries        []Summary         `json:"summaries,omitempty"`
	EvidencePointers []EvidencePointer `json:"evidence_pointers,omitempty"`
}
