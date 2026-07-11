package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFailsClosedWithoutAgentNexusServiceSecret(t *testing.T) {
	t.Setenv("ATLAS_CONFIG", "")
	t.Setenv("ATLAS_NEXUS_SERVICE_SECRET_FILE", filepath.Join(t.TempDir(), "missing.secret"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "AgentNexus service credential") {
		t.Fatalf("startup error=%v", err)
	}
}
