// Package artifacts runs the parse pipeline: raw artifact in object storage
// -> parser gateway -> AtlasDocument (full doc to object storage) ->
// sanitized summaries/blocks + evidence pointer + index job in PostgreSQL.
package artifacts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

const (
	JobTypeArtifact = "artifact_processing"

	// Data boundary limits (mirror the DB check constraints).
	maxExcerptLen = 512
	maxSummaryLen = 4000
)

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
