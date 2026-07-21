// Package nexusclient is the only doorway from AgentAtlas to enterprise
// resources. It implements the sdk/go/nexus.Client contract over HTTP against
// the AgentNexus proposal API, plus an in-memory mock for unit tests.
package nexusclient

import "errors"

// ErrDenied is returned when AgentNexus refuses an operation (HTTP 403).
// Callers must fail closed: no fallback reads, no partial answers built on
// denied evidence.
var ErrDenied = errors.New("agentnexus: denied")

// ErrConflict is a permanent idempotency-key/canonical-payload conflict.
var ErrConflict = errors.New("agentnexus: idempotency conflict")

// ErrOrgCursorAhead reports that the org-event cursor is ahead of the tenant's
// sealed organization version (the contract's declared 409 on
// GET /v1/org-events). It is a corrupt-consumer condition, not a transient
// fault: retrying the same cursor can never succeed, so callers must stop and
// reset it rather than loop.
var ErrOrgCursorAhead = errors.New("agentnexus: org event cursor is ahead of the sealed org version")
