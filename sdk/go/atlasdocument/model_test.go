package atlasdocument

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAtlasDocumentRoundtrip(t *testing.T) {
	doc := AtlasDocument{
		AtlasDocumentID: "doc_1",
		ArtifactID:      "file_1",
		SourceHash:      "sha256:abc",
		ContentType:     "application/pdf",
		Provider:        "docling",
		Confidence:      0.92,
		Blocks: []Block{
			{BlockID: "b1", Type: BlockHeading, Order: 0, Page: 1, Text: "MES 异常工单排查"},
			{BlockID: "b2", Type: BlockText, Order: 1, Page: 1, Text: "步骤一……"},
		},
		Summaries: []Summary{
			{SummaryID: "s1", Level: SummaryDisplay, Text: "MES 异常工单排查 SOP"},
		},
		EvidencePointers: []EvidencePointer{
			{
				EnterpriseID: "ent_1",
				ResourceType: "document",
				ResourceRef:  "fs://sop/mes.pdf",
				SourceSystem: "filesystem",
				ContentHash:  "sha256:abc",
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round AtlasDocument
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(doc, round) {
		t.Fatalf("roundtrip mismatch:\n%+v\n%+v", doc, round)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	for _, key := range []string{"atlas_document_id", "artifact_id", "source_hash", "content_type", "blocks", "summaries", "evidence_pointers"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing schema field %q", key)
		}
	}
}
