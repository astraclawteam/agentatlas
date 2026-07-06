// Package app hosts shared service runtime pieces: health status, versioning,
// and (from Goal 12 on) the HTTP handler surface.
package app

// Version is the open-core runtime version stamped into health status and logs.
const Version = "0.1.0-dev"

// HealthStatus describes a service identity and its readiness. Dependencies
// lists the names of external systems the service needs; readiness stays
// false until all of them are confirmed reachable.
type HealthStatus struct {
	Service      string   `json:"service"`
	Version      string   `json:"version"`
	Ready        bool     `json:"ready"`
	Dependencies []string `json:"dependencies"`
}

func NewHealthStatus(service string, dependencies ...string) HealthStatus {
	return HealthStatus{
		Service:      service,
		Version:      Version,
		Ready:        false,
		Dependencies: dependencies,
	}
}

// MarkReady returns a copy with readiness set. Copy semantics keep the zero
// value trustworthy for concurrent readers.
func (h HealthStatus) MarkReady(ready bool) HealthStatus {
	h.Ready = ready
	return h
}
