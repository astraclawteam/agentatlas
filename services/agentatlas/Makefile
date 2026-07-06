.PHONY: build test tidy generate

build:
	go build ./...

test:
	go test ./...

tidy:
	go mod tidy

# Regenerate committed OpenAPI types (api/openapi/gen). Idempotent; CI runs
# the contract drift test to catch uncommitted regeneration.
generate:
	go tool oapi-codegen -config api/openapi/oapi-codegen-runtime.yaml api/openapi/atlas-runtime.yaml
	go tool oapi-codegen -config api/openapi/oapi-codegen-agent.yaml api/openapi/atlas-agent.yaml
