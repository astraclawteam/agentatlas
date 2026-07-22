package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
)

// atlas-agent served GET /healthz as
//
//	{"service":"atlas-agent","ready":true,
//	 "dependencies":["postgres","nats","agentnexus","llmrouter"],...}
//
// and GET /readyz as "404 page not found". The ready flag was a constant and
// the four dependency names were string literals; when that body was observed,
// llmrouter pointed at host.docker.internal:8000 (which resolves to nothing
// inside the stack) and AgentNexus had never been contacted at all, because
// atlas-agent resolves that link lazily at first use.
//
// Naming four dependencies makes a constant read as a checked result. These
// assertions go over real HTTP against the composed router; they deliberately
// do not read the router's source text.

func probeOK(name string) DependencyProbe {
	return DependencyProbe{Name: name, Check: func(context.Context) error { return nil }}
}

func probeFailing(name string, err error) DependencyProbe {
	return DependencyProbe{Name: name, Check: func(context.Context) error { return err }}
}

type healthBody struct {
	Service      string   `json:"service"`
	Ready        bool     `json:"ready"`
	Dependencies []string `json:"dependencies"`
	Unready      []string `json:"unready"`
	Reason       string   `json:"reason"`
}

// agentHealth drives one health path through the real atlas-agent router.
func agentHealth(t *testing.T, path string, probes []DependencyProbe) (int, healthBody, string) {
	t.Helper()
	router := NewAgentRouter(AgentRouterDeps{
		OrgAuthorization:    &allowOrgAuthorization{},
		ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"},
		ApprovalAuthority:   "oa.example",
		WorkCaseContextFor:  alwaysWorkCaseBacked,
		Nexus:               adminMock(),
		Dependencies:        probes,
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	var body healthBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s body %q: %v", path, recorder.Body.String(), err)
	}
	return recorder.Code, body, recorder.Body.String()
}

// The endpoint AgentNexus's gateway agent probes must exist. Every AgentAtlas
// entry in AGENTNEXUS_HEALTH_PROBE_TARGETS used to answer 404, which that
// service's diagnostics reported as an unreachable peer.
func TestReadyzExistsInsteadOfAnsweringNotFound(t *testing.T) {
	status, _, raw := agentHealth(t, "/readyz", []DependencyProbe{probeOK("postgres")})
	if status == http.StatusNotFound {
		t.Fatalf("/readyz still answers 404: %s", raw)
	}
}

// The dependency list must be the probes, not a literal. This is the assertion
// that fails if the hard-coded ["postgres","nats","agentnexus","llmrouter"]
// comes back: a router given two probes may name exactly those two.
func TestHealthSurfaceNamesOnlyProbedDependencies(t *testing.T) {
	probes := []DependencyProbe{probeOK("postgres"), probeOK("nats")}
	for _, path := range []string{"/healthz", "/readyz"} {
		_, body, raw := agentHealth(t, path, probes)
		if strings.Join(body.Dependencies, ",") != "nats,postgres" {
			t.Errorf("%s names dependencies %v; it may name only what it probes: %s", path, body.Dependencies, raw)
		}
	}
}

// The core of the defect: a dependency that does not answer must make the
// service unready on BOTH surfaces, and /readyz must say which one.
func TestAnUnreachableDependencyIsNotReportedAsReady(t *testing.T) {
	probes := []DependencyProbe{
		probeOK("postgres"),
		probeOK("nats"),
		probeFailing("agentnexus", errors.New("dial tcp: no such host")),
		probeFailing("llmrouter", errors.New("dial tcp: no such host")),
	}

	// Liveness stays 200 -- the container must not flap -- but the body must
	// stop claiming readiness it has not checked.
	status, body, raw := agentHealth(t, "/healthz", probes)
	if status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 so the process stays observable", status)
	}
	if body.Ready {
		t.Errorf("/healthz reports ready:true while two of its four named dependencies do not answer: %s", raw)
	}

	status, body, raw = agentHealth(t, "/readyz", probes)
	if status != http.StatusServiceUnavailable || body.Ready {
		t.Fatalf("/readyz status=%d ready=%v with two dependencies unreachable: %s", status, body.Ready, raw)
	}
	if strings.Join(body.Unready, ",") != "agentnexus,llmrouter" {
		t.Errorf("/readyz reports unready %v; want exactly the two that failed: %s", body.Unready, raw)
	}
}

// The positive case, so the assertions above cannot be satisfied by a surface
// that reports false unconditionally.
func TestEveryProbePassingIsReady(t *testing.T) {
	probes := []DependencyProbe{probeOK("postgres"), probeOK("agentnexus")}
	if status, body, raw := agentHealth(t, "/readyz", probes); status != http.StatusOK || !body.Ready {
		t.Fatalf("/readyz status=%d ready=%v with every probe passing: %s", status, body.Ready, raw)
	}
	if _, body, raw := agentHealth(t, "/healthz", probes); !body.Ready {
		t.Fatalf("/healthz ready=false with every probe passing: %s", raw)
	}
}

// A registered name with nothing behind it is the exact shape being replaced,
// so it counts as failed rather than as a pass.
func TestARegisteredNameWithNoCheckIsUnready(t *testing.T) {
	status, body, raw := agentHealth(t, "/readyz", []DependencyProbe{{Name: "agentnexus"}})
	if status != http.StatusServiceUnavailable || body.Ready {
		t.Fatalf("a dependency with no check was reported ready: status=%d %s", status, raw)
	}
}

// The endpoint is unauthenticated. A probe error routinely carries the
// configured address of an internal system, and llmrouter's carries an
// Authorization header echo in some deployments; only names may be published.
func TestUnreadyBodyPublishesNamesNotProbeErrors(t *testing.T) {
	const leak = "postgres://atlas:hunter2@postgres.internal:5432/agentatlas"
	_, body, raw := agentHealth(t, "/readyz", []DependencyProbe{probeFailing("postgres", errors.New(leak))})
	if strings.Contains(raw, "hunter2") || strings.Contains(raw, "postgres.internal") {
		t.Fatalf("/readyz published a probe error: %s", raw)
	}
	if strings.Join(body.Unready, ",") != "postgres" {
		t.Fatalf("unready = %v, want the name alone", body.Unready)
	}
}

// A composition root that registers no probes must not be able to serve a
// surface that names nothing and is unconditionally ready.
func TestMissingRequiredReportsAnEmptyProbeSet(t *testing.T) {
	if !contains(AgentRouterDeps{}.MissingRequired(), "Dependencies") {
		t.Fatal("a deps with no readiness probes is not reported incomplete")
	}
	deps := AgentRouterDeps{Dependencies: []DependencyProbe{probeOK("postgres")}}
	if contains(deps.MissingRequired(), "Dependencies") {
		t.Fatal("a deps with probes is still reported as missing them")
	}
}

// A dependency that never answers must resolve through the probe deadline and
// count as unready, rather than making the endpoint itself the thing that
// hangs. The request context is already cancelled here so the bound is proven
// without a five-second test.
func TestAHungProbeIsBoundedAndCountsAsUnready(t *testing.T) {
	hung := DependencyProbe{Name: "agentnexus", Check: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if failed := unreadyDependencies(ctx, []DependencyProbe{hung}); strings.Join(failed, ",") != "agentnexus" {
		t.Fatalf("failed = %v, want the hung dependency", failed)
	}
}
