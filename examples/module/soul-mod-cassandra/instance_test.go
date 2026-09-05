package main

import (
	"errors"
	"strings"
	"testing"
)

func TestInstancePinged_ReportsVersionAndTopology(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return []map[string]any{{
			"release_version": "5.0.2",
			"cluster_name":    "app-cluster",
			"data_center":     "dc1",
			"rack":            "rack1",
			// A uuid arrives as its own type, not a string, and Output has to carry
			// the value rather than the Go type.
			"host_id": [2]int{0, 0},
		}}, nil
	}}
	stream := apply(t, newModule(s).instance(), "pinged", connParams())

	// A probe is not a mutation: changed=false by design.
	e := assertOutcome(t, stream, false, false)
	fields := e.GetOutput().GetFields()
	if !fields["ok"].GetBoolValue() {
		t.Error("a reachable node must report ok=true")
	}
	if got := fields["release_version"].GetStringValue(); got != "5.0.2" {
		t.Errorf("release_version = %q, want 5.0.2", got)
	}
	if got := fields["data_center"].GetStringValue(); got != "dc1" {
		t.Errorf("data_center = %q, want dc1", got)
	}
	if fields["host_id"].GetStringValue() == "" {
		t.Error("host_id must be rendered even when the driver hands back a non-string type")
	}
}

func TestInstancePinged_FailsWhenTheNodeDoesNotAnswer(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return nil, errors.New("no connections available")
	}}
	stream := apply(t, newModule(s).instance(), "pinged", connParams())

	assertOutcome(t, stream, false, true)
	assertNoSecretInEvents(t, stream)
}

// TestInstancePinged_FailsOnAnEmptySystemLocal — a node that accepts a connection and
// answers nothing for itself is not serving, and reporting ok=true on it would make
// the probe useless as a health gate.
func TestInstancePinged_FailsOnAnEmptySystemLocal(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).instance(), "pinged", connParams())

	e := assertOutcome(t, stream, false, true)
	if !strings.Contains(e.GetMessage(), "system.local") {
		t.Errorf("the failure must name what it read, got %q", e.GetMessage())
	}
}

// TestConnectionCarriesTheDeclaredEndpoints — contact points and the default port
// reach the driver as written.
func TestConnectionCarriesTheDeclaredEndpoints(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).instance(), "pinged", connParams())

	if len(s.cfg.hosts) != 2 || s.cfg.hosts[0] != "10.0.0.1" || s.cfg.hosts[1] != "10.0.0.2:9042" {
		t.Errorf("contact points did not reach the driver: %v", s.cfg.hosts)
	}
	if s.cfg.port != defaultPort {
		t.Errorf("port = %d, want the %d default", s.cfg.port, defaultPort)
	}
	if s.cfg.username != "cassandra" {
		t.Errorf("username = %q", s.cfg.username)
	}
}
