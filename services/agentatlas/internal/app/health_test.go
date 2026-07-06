package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNewHealthStatus(t *testing.T) {
	h := NewHealthStatus("atlas-api", "postgres", "opensearch", "nats")
	if h.Service != "atlas-api" {
		t.Fatalf("service = %q", h.Service)
	}
	if h.Version != Version {
		t.Fatalf("version = %q, want %q", h.Version, Version)
	}
	if h.Ready {
		t.Fatal("new health status must not be ready")
	}
	if len(h.Dependencies) != 3 || h.Dependencies[1] != "opensearch" {
		t.Fatalf("dependencies = %v", h.Dependencies)
	}

	ready := h.MarkReady(true)
	if !ready.Ready {
		t.Fatal("MarkReady(true) not applied")
	}
	if h.Ready {
		t.Fatal("MarkReady must not mutate the receiver")
	}

	raw, err := json.Marshal(ready)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round HealthStatus
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(round, ready) {
		t.Fatalf("json roundtrip mismatch: %+v vs %+v", round, ready)
	}
}
