// Package schemas embeds the public JSON Schemas so runtime validation always
// uses the exact contract files shipped in this repository.
package schemas

import _ "embed"

//go:embed workflow/workflow.schema.json
var WorkflowSchemaJSON []byte

//go:embed parser/parser-provider.schema.json
var ParserProviderSchemaJSON []byte

//go:embed atlasdocument/atlas-document.schema.json
var AtlasDocumentSchemaJSON []byte

//go:embed dream/dream.schema.json
var DreamSchemaJSON []byte

//go:embed governance/governance.schema.json
var GovernanceSchemaJSON []byte

//go:embed workcase/workcase.schema.json
var WorkCaseSchemaJSON []byte

//go:embed workcase/work-plan.schema.json
var WorkPlanSchemaJSON []byte

//go:embed workcase/action.schema.json
var ActionSpecSchemaJSON []byte

//go:embed outcome/outcome.schema.json
var OutcomeSchemaJSON []byte

//go:embed outcome/lineage.schema.json
var LineageSchemaJSON []byte
