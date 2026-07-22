//go:build e2e_live

package harness

// YAML loader for post-apply expectations (L3b-5).
//
// Format is an extension of L3a (see tests/e2e/harness/fixtures.go +
// docs/testing/e2e.md): alongside apply_runs / incarnation_state /
// audit_events / metrics, a new host_state section is added — per-soul
// container-side expectations (packages / services / files), checked by
// running Exec inside the privileged Debian-12 soul container via the
// AssertHost*-methods (L3b-4).
//
// LoadExpectations reads the YAML from disk and validates the structure
// (plain yaml.Unmarshal, no hidden transforms). AssertExpectations applies
// the whole set of checks in one call — the caller doesn't deal with
// staged validation. parseMetricThreshold handles constraint expressions
// like ">= 1" / "== 0" / "> 3" / "1" (a bare number is equivalent to
// ">= number").

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Expectations — post-apply expectations for a single scenario run (format
// of `<test>/expectations/<phase>.yaml`).
//
// apply_runs/incarnation_state/audit_events/metrics are symmetric with the
// L3a fixture (tests/e2e/harness/fixtures.ExpectationsAfter); host_state is
// a new L3b section for container-side expectations.
type Expectations struct {
	ApplyRuns        ApplyRunsExpectation    `yaml:"apply_runs"`
	IncarnationState map[string]any          `yaml:"incarnation_state"`
	AuditEvents      []AuditEventExpectation `yaml:"audit_events"`
	Metrics          map[string]string       `yaml:"metrics"`
	HostState        []HostStateExpectation  `yaml:"host_state"`
}

// ApplyRunsExpectation — expected shape of apply_runs.status (value from the
// real enum keeper/internal/applyrun).
type ApplyRunsExpectation struct {
	Status string `yaml:"status"`
}

// AuditEventExpectation — presence check against audit_log (type is
// required, payload is a deep subset).
type AuditEventExpectation struct {
	Type    string         `yaml:"type"`
	Payload map[string]any `yaml:"payload"`
}

// HostStateExpectation — container-side expectations for one soul host.
//
// Soul is an FQDN, must match one of Stack.SoulContainers[i].SID;
// AssertExpectations resolves the index via findSoulIdx and fails if the
// sid is unknown (helps catch typos in the YAML).
type HostStateExpectation struct {
	Soul      string                    `yaml:"soul"`
	Packages  map[string]string         `yaml:"packages"` // pkg -> status (only "installed" in MVP)
	Services  map[string]string         `yaml:"services"` // svc -> state  (only "active"    in MVP)
	Files     []HostFileExpectation     `yaml:"files"`
	Endpoints []HostEndpointExpectation `yaml:"endpoints"`
}

// HostFileExpectation — expectation about a file inside the container.
//
// Path is required (AssertHostFileExists); Contains is optional: if
// non-empty, AssertHostFileContent is additionally checked.
type HostFileExpectation struct {
	Path     string `yaml:"path"`
	Contains string `yaml:"contains"`
}

// HostEndpointExpectation — HTTP expectation for a network service inside
// the container (AssertHostHTTPContains). URL is required, Contains is
// required (the response body must contain the substring — e.g.
// node_exporter :9100/metrics -> "node_"). This proves the port is actually
// listening and serving the expected content — something services/files
// checks alone don't give.
type HostEndpointExpectation struct {
	URL      string `yaml:"url"`
	Contains string `yaml:"contains"`
}

// LoadExpectations reads an expectations YAML file from disk and parses it
// into a typed structure. Strict mode: KnownFields(true) — extra keys
// (schema typos) are caught at test startup, not at the assert phase.
func LoadExpectations(t *testing.T, path string) *Expectations {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("LoadExpectations(%s): read: %v", path, err)
	}
	var e Expectations
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("LoadExpectations(%s): yaml decode: %v", path, err)
	}
	if e.ApplyRuns.Status != "" {
		CheckApplyRunsStatusValid(t, e.ApplyRuns.Status)
	}
	return &e
}

// AssertExpectations applies all expectation blocks against the stand in a
// deterministic order: apply_runs -> incarnation_state -> audit_events ->
// metrics -> host_state. Each block is a separate assert method; on failure
// of one, t.Fatal happens inside that method (short-circuit — no point
// continuing).
func (s *Stack) AssertExpectations(t *testing.T, e *Expectations, applyID, incName string) {
	t.Helper()

	if e.ApplyRuns.Status != "" {
		s.AssertApplyRunsStatus(t, applyID, e.ApplyRuns.Status)
	}

	if len(e.IncarnationState) > 0 {
		s.AssertIncarnationState(t, incName, e.IncarnationState)
	}

	for _, ae := range e.AuditEvents {
		s.AssertAuditEvent(t, ae.Type, ae.Payload)
	}

	for query, expr := range e.Metrics {
		threshold, op, err := parseMetricThreshold(expr)
		if err != nil {
			t.Fatalf("AssertExpectations: metric %q: %v", query, err)
		}
		switch op {
		case ">=":
			s.AssertMetricGE(t, query, threshold)
		default:
			// MVP: only `>=` is supported (same as L3a/L3b as of L3b-5).
			// Other operators are a non-breaking extension of the YAML
			// format.
			t.Fatalf("AssertExpectations: metric %q: operator %q not supported yet (only '>=')",
				query, op)
		}
	}

	for _, hs := range e.HostState {
		soulIdx := s.findSoulIdx(hs.Soul)
		if soulIdx < 0 {
			knownSIDs := make([]string, 0, len(s.SoulContainers))
			for _, sc := range s.SoulContainers {
				knownSIDs = append(knownSIDs, sc.SID)
			}
			t.Fatalf("host_state: soul %q not found in Stack.SoulContainers (known=%v)",
				hs.Soul, knownSIDs)
		}
		for pkg, status := range hs.Packages {
			switch status {
			case "installed":
				s.AssertHostPkgInstalled(t, soulIdx, pkg)
			default:
				t.Fatalf("host_state(%s).packages[%s]: status %q not supported (only 'installed')",
					hs.Soul, pkg, status)
			}
		}
		for svc, state := range hs.Services {
			switch state {
			case "active":
				s.AssertHostServiceActive(t, soulIdx, svc)
			default:
				t.Fatalf("host_state(%s).services[%s]: state %q not supported (only 'active')",
					hs.Soul, svc, state)
			}
		}
		for _, f := range hs.Files {
			if f.Path == "" {
				t.Fatalf("host_state(%s).files: empty path", hs.Soul)
			}
			s.AssertHostFileExists(t, soulIdx, f.Path)
			if f.Contains != "" {
				s.AssertHostFileContent(t, soulIdx, f.Path, f.Contains)
			}
		}
		for _, ep := range hs.Endpoints {
			if ep.URL == "" || ep.Contains == "" {
				t.Fatalf("host_state(%s).endpoints: url and contains are required (url=%q contains=%q)",
					hs.Soul, ep.URL, ep.Contains)
			}
			// 30s retry: the network service (node_exporter) binds its
			// listen socket asynchronously after systemctl start.
			s.AssertHostHTTPContains(t, soulIdx, ep.URL, ep.Contains, 30)
		}
	}
}

// findSoulIdx returns the index of the SoulContainer by SID, or -1 if none.
func (s *Stack) findSoulIdx(sid string) int {
	for i, sc := range s.SoulContainers {
		if sc != nil && sc.SID == sid {
			return i
		}
	}
	return -1
}

// parseMetricThreshold parses a constraint expression like ">= 1" / "== 0" /
// "> 3" or a bare number "1" (treated as ">= 1"). Returns (value, operator,
// error). The operator is normalized to one of {">=", "==", ">", "<=", "<"};
// for a bare number it's ">=".
func parseMetricThreshold(expr string) (float64, string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, "", fmt.Errorf("empty constraint expression")
	}

	for _, op := range []string{">=", "<=", "==", ">", "<"} {
		if strings.HasPrefix(expr, op) {
			rest := strings.TrimSpace(expr[len(op):])
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return 0, "", fmt.Errorf("number after %q does not parse: %q", op, rest)
			}
			return v, op, nil
		}
	}

	v, err := strconv.ParseFloat(expr, 64)
	if err != nil {
		return 0, "", fmt.Errorf("neither an operator nor a bare number: %q", expr)
	}
	return v, ">=", nil
}
