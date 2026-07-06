// Package parser mirrors schemas/parser/parser-provider.schema.json.
// Parser providers (open-core sidecars and enterprise extensions) declare
// their capabilities with these types.
package parser

// OutputSchemaV1 is the only AtlasDocument output schema in v1.
const OutputSchemaV1 = "atlas_document.v1"

type Provider struct {
	ProviderID      string   `json:"provider_id"`
	Capabilities    []string `json:"capabilities"`
	InputTypes      []string `json:"input_types"`
	OutputSchema    string   `json:"output_schema"`
	MaxFileSizeMB   int      `json:"max_file_size_mb"`
	SupportsPrivate bool     `json:"supports_private"`
}

// Common capability identifiers used by the built-in sidecars.
const (
	CapPDFLayout   = "pdf.layout"
	CapPDFTable    = "pdf.table"
	CapImageOCR    = "image.ocr"
	CapOfficeParse = "office.parse"
	CapAudioASR    = "audio.asr"
	CapVideoScene  = "video.scene"
)
