package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/astraclawteam/agentatlas/services/agentatlas/schemas"
)

// Validator validates workflow documents against the embedded public schema.
type Validator struct {
	schema *jsonschema.Schema
}

func NewValidator() (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.WorkflowSchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("workflow schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const name = "workflow.schema.json"
	if err := c.AddResource(name, doc); err != nil {
		return nil, fmt.Errorf("workflow schema: %w", err)
	}
	sch, err := c.Compile(name)
	if err != nil {
		return nil, fmt.Errorf("workflow schema: %w", err)
	}
	return &Validator{schema: sch}, nil
}

// ValidateBytes validates a raw workflow JSON document.
func (v *Validator) ValidateBytes(raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("workflow document: %w", err)
	}
	if err := v.schema.Validate(doc); err != nil {
		return fmt.Errorf("workflow schema violation: %w", err)
	}
	return nil
}

// Validate validates a typed definition.
func (v *Validator) Validate(def Definition) error {
	raw, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encode workflow: %w", err)
	}
	return v.ValidateBytes(raw)
}
