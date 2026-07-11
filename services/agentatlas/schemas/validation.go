package schemas

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func validateDefinition(schemaJSON []byte, resource, name string, data []byte) (map[string]any, error) {
	var schemaDoc any
	if err := json.Unmarshal(schemaJSON, &schemaDoc); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, schemaDoc); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(resource + "#/$defs/" + name)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if err := schema.Validate(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func ValidateDreamPolicy(data []byte) error {
	doc, err := validateDefinition(DreamSchemaJSON, "dream.schema.json", "DreamPolicyDefinition", data)
	if err != nil {
		return err
	}
	tz := doc["timezone"].(string)
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("timezone must be an installed IANA name: %w", err)
	}
	if _, err := cron.ParseStandard(doc["schedule"].(string)); err != nil {
		return fmt.Errorf("schedule must be a valid five-field cron: %w", err)
	}
	return nil
}

func ValidateDreamRun(data []byte) error {
	_, err := validateDefinition(DreamSchemaJSON, "dream.schema.json", "DreamRunView", data)
	return err
}

func ValidateDreamSummary(data []byte) error {
	_, err := validateDefinition(DreamSchemaJSON, "dream.schema.json", "DreamSummaryView", data)
	return err
}

func ValidateRiskAssessment(data []byte) error {
	_, err := validateDefinition(GovernanceSchemaJSON, "governance.schema.json", "RiskAssessment", data)
	return err
}

func ValidateReviewRoute(data []byte) error {
	doc, err := validateDefinition(GovernanceSchemaJSON, "governance.schema.json", "ReviewRoute", data)
	if err != nil {
		return err
	}
	requester, _ := doc["requester_user_id"].(string)
	reviewer, _ := doc["reviewer_user_id"].(string)
	if reviewer != "" && requester == reviewer {
		return fmt.Errorf("requester cannot review their own change")
	}
	return nil
}

func ValidateChangeDraft(data []byte) error {
	_, err := validateDefinition(GovernanceSchemaJSON, "governance.schema.json", "ChangeDraft", data)
	return err
}
