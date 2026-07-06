package parser

import (
	"encoding/json"
	"testing"
)

func TestProviderJSONFieldNames(t *testing.T) {
	p := Provider{
		ProviderID:      "docling",
		Capabilities:    []string{CapPDFLayout, CapPDFTable, CapImageOCR, CapOfficeParse},
		InputTypes:      []string{"application/pdf", "image/png"},
		OutputSchema:    OutputSchemaV1,
		MaxFileSizeMB:   200,
		SupportsPrivate: true,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"provider_id", "capabilities", "input_types", "output_schema", "max_file_size_mb", "supports_private"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing schema field %q in %s", key, raw)
		}
	}
	if m["output_schema"] != OutputSchemaV1 {
		t.Fatalf("output_schema = %v", m["output_schema"])
	}
}
