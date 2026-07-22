package app

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// DependencyProbe answers, for ONE named dependency, whether this process can
// actually reach and use it right now.
//
// The name is not decoration. The health surface publishes exactly the names
// registered here, so a dependency cannot be listed unless something is
// prepared to check it. atlas-agent used to publish
// ["postgres","nats","agentnexus","llmrouter"] beside a constant ready:true and
// no /readyz at all: four names that read as four checked results and were four
// string literals. When that body was served, llmrouter pointed at
// host.docker.internal:8000 (which resolves to nothing inside the stack) and
// AgentNexus had never been contacted, because atlas-agent resolves that link
// lazily at first use.
type DependencyProbe struct {
	Name string
	// Check reports nil when the dependency is reachable and usable. Its error
	// is a server-side diagnostic and is NEVER published: see readyzResponse.
	Check func(context.Context) error
}

// dependencyProbeTimeout bounds a whole readiness evaluation. Probes run
// concurrently under one deadline, so a single hung dependency cannot make the
// endpoint itself the thing that times out.
const dependencyProbeTimeout = 5 * time.Second

// readyzResponse is the readiness answer. It embeds HealthStatus so /readyz and
// /healthz report the same service/version/ready/dependencies fields, and
// carries the same "reason" the composition root states for a process that
// started degraded.
//
// Unready carries dependency NAMES only. This endpoint is unauthenticated and a
// probe error routinely contains the configured address of an internal system;
// the name is what an operator needs to know which link to look at, and the
// full error belongs in that service's own logs.
type readyzResponse struct {
	HealthStatus
	TLS     *transportsecurity.Status `json:"tls,omitempty"`
	Reason  string                    `json:"reason,omitempty"`
	Unready []string                  `json:"unready,omitempty"`
}

// probeNames returns the registered dependency names, sorted. This is the ONLY
// source of the names the health surface publishes.
func probeNames(probes []DependencyProbe) []string {
	names := make([]string, 0, len(probes))
	for _, probe := range probes {
		names = append(names, probe.Name)
	}
	sort.Strings(names)
	return names
}

// unreadyDependencies runs every probe and returns the names that failed,
// sorted. A probe with no Check counts as failed: a registered name that
// nothing answers is the defect this type exists to prevent, not a pass.
func unreadyDependencies(ctx context.Context, probes []DependencyProbe) []string {
	ctx, cancel := context.WithTimeout(ctx, dependencyProbeTimeout)
	defer cancel()
	var (
		mu     sync.Mutex
		failed []string
		wg     sync.WaitGroup
	)
	for _, probe := range probes {
		wg.Add(1)
		go func(probe DependencyProbe) {
			defer wg.Done()
			if probe.Check != nil {
				if err := probe.Check(ctx); err == nil {
					return
				}
			}
			mu.Lock()
			failed = append(failed, probe.Name)
			mu.Unlock()
		}(probe)
	}
	wg.Wait()
	sort.Strings(failed)
	return failed
}

// readinessSurface is everything a service's health endpoints answer from.
type readinessSurface struct {
	Service string
	Probes  []DependencyProbe
	TLS     *transportsecurity.Manager
	// NotReadyReason is the composition root's own statement that this process
	// started degraded (an unconfigured llmrouter, for instance). It is a
	// STARTUP fact and the probes are a LIVE one; both make the service
	// unready, and neither substitutes for the other.
	NotReadyReason string
}

// evaluateReadiness is the ONE readiness derivation for a service. /healthz and
// /readyz both read it, because the thing that must never happen again is two
// surfaces on one process answering from two different facts.
//
// A certificate problem is reported distinctly (through the "tls" field's own
// detail) from dependency reachability, but it still fails readiness closed.
func evaluateReadiness(ctx context.Context, surface readinessSurface) readyzResponse {
	resp := readyzResponse{
		Reason:  surface.NotReadyReason,
		Unready: unreadyDependencies(ctx, surface.Probes),
	}
	ready := len(resp.Unready) == 0 && resp.Reason == ""
	if surface.TLS != nil {
		status := surface.TLS.Status()
		resp.TLS = &status
		ready = ready && status.Ready
	}
	resp.HealthStatus = NewHealthStatus(surface.Service, probeNames(surface.Probes)...).MarkReady(ready)
	return resp
}

// mountHealthAndReadiness registers the pair.
//
// /healthz is LIVENESS: always HTTP 200 so the container does not flap and the
// process stays observable. Its body still reports the honest readiness value,
// because health.go's own contract says readiness stays false until every named
// dependency is confirmed reachable -- the constant true was a violation of a
// rule this repo had already written down.
//
// /readyz is READINESS: 503 with the failing names when it cannot serve. It
// used to 404, which also broke the other direction -- AgentNexus's gateway
// agent probes peer /readyz endpoints (AGENTNEXUS_HEALTH_PROBE_TARGETS) and
// every AgentAtlas target answered "404 page not found".
func mountHealthAndReadiness(mount func(pattern string, handler http.HandlerFunc), surface readinessSurface) {
	mount("/healthz", func(w http.ResponseWriter, r *http.Request) {
		result := evaluateReadiness(r.Context(), surface)
		writeJSON(w, http.StatusOK, healthzResponse{
			HealthStatus: result.HealthStatus, TLS: result.TLS, Reason: result.Reason,
		})
	})
	mount("/readyz", func(w http.ResponseWriter, r *http.Request) {
		result := evaluateReadiness(r.Context(), surface)
		status := http.StatusOK
		// Branch on the evaluated value rather than re-deriving the predicate:
		// independently derived readiness is exactly how two surfaces on one
		// process came to contradict each other.
		if !result.Ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, result)
	})
}
