.PHONY: build test tidy generate verify-contracts docker-build-check

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

verify-contracts:
	go test ./schemas ./internal/app -run 'Dream|Governance|ReviewRoute|ChangeDraft|Contract|GeneratedAgentTypes' -count=1

# Builds real service images. `build`/`test` above pass on a developer machine
# even when no image can be built, because go.mod reaches into a sibling
# agentnexus checkout that no build context contains; only this target catches
# that. SERVICES limits the set (default: every cmd/).
docker-build-check:
	sh deploy/docker/build-check.sh $(SERVICES)
