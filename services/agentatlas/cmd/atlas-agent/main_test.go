package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
)

func TestRunFailsClosedWithoutAgentNexusServiceSecret(t *testing.T) {
	t.Setenv("ATLAS_CONFIG", "")
	t.Setenv("ATLAS_APPROVAL_AUTHORITY", "oa.example")
	t.Setenv("ATLAS_NEXUS_SERVICE_SECRET_FILE", filepath.Join(t.TempDir(), "missing.secret"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "AgentNexus service credential") {
		t.Fatalf("startup error=%v", err)
	}
}

// Configuration is rejected before the process touches PostgreSQL. A run that
// is going to refuse its own configuration must not have applied 22 migrations
// on the way out: the observed failure ended with
//
//	goose: successfully migrated database to version: 22
//	atlas-agent: llmroutermodel: BaseURL is required
//
// which left a schema behind for a service that never served a request. The
// DSN below points at a port nothing listens on, so if validation ever moves
// back behind storage.Migrate this test reports a PostgreSQL error instead.
func TestRunValidatesConfigBeforeTouchingPostgres(t *testing.T) {
	t.Setenv("ATLAS_CONFIG", "")
	t.Setenv("ATLAS_APPROVAL_AUTHORITY", "")
	t.Setenv("ATLAS_POSTGRES_DSN", "postgres://atlas:atlas@127.0.0.1:1/agentatlas?sslmode=disable&connect_timeout=1")
	err := run()
	if err == nil {
		t.Fatal("run() succeeded with no approval authority")
	}
	if !strings.Contains(err.Error(), "approval_authority") {
		t.Fatalf("startup error=%v, want the approval-authority gap (reached storage before validating config?)", err)
	}
}

// An unset llmrouter endpoint degrades: no runner, a stated reason, and NO
// error. Exiting here is what AgentNexus's gateway-agent deliberately does not
// do for the identical gap, and two products in one stack answering "no model
// configured" with `exit 1` and `degrade with a reason` is the inconsistency
// this test pins down.
func TestComposeAgentRunnerDegradesWithoutAnEndpoint(t *testing.T) {
	runner, llm, reason, err := composeAgentRunner(config.LLMRouter{}, "deepseek-v4-flash", nil)
	if err != nil {
		t.Fatalf("err = %v, want a degraded start", err)
	}
	if runner != nil {
		t.Fatal("composed a runner with no endpoint")
	}
	if llm != nil {
		t.Fatal("returned a model to probe against an endpoint that does not exist")
	}
	if !strings.Contains(reason, "ATLAS_LLMROUTER_BASE_URL") {
		t.Fatalf("reason = %q, want the variable an operator must set", reason)
	}
}

func TestComposeAgentRunnerBuildsWithAnEndpoint(t *testing.T) {
	runner, llm, reason, err := composeAgentRunner(config.LLMRouter{BaseURL: "http://llmrouter.invalid:8000/v1"}, "deepseek-v4-flash", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if runner == nil {
		t.Fatal("no runner despite a configured endpoint")
	}
	// The readiness surface must probe the SAME endpoint the runner calls, so
	// the model comes back with it rather than being rebuilt for the probe.
	if llm == nil {
		t.Fatal("no model to probe despite a configured endpoint")
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
}

// The endpoint has no default and the model name does. That asymmetry is
// deliberate: a wrong model name comes back as a router error, an endpoint that
// resolves to nothing comes back as a service that reports healthy and fails at
// the first call.
func TestResolvedDefaultModel(t *testing.T) {
	if got := resolvedDefaultModel(config.LLMRouter{}); got != "deepseek-v4-flash" {
		t.Fatalf("unset model = %q", got)
	}
	if got := resolvedDefaultModel(config.LLMRouter{DefaultModel: "qwen3-max"}); got != "qwen3-max" {
		t.Fatalf("configured model = %q", got)
	}
}
