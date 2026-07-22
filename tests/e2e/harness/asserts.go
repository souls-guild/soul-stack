//go:build e2e

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// validApplyRunsStatus — the closed set of apply_runs.status values allowed
// in the harness's YAML expectations.
//
// Source of truth — keeper/internal/applyrun/applyrun.go (Status consts +
// ValidStatus). Duplicated here as a literal because tests/e2e is a separate
// go module without a dependency on keeper/ (testcontainers deps must not leak
// into the main modules, per the pilot). In the L3a-implementation slice —
// replace with an import of
// `github.com/souls-guild/soul-stack/keeper/internal/applyrun` via a replace
// in go.mod and `applyrun.ValidStatus(...)` instead of the literal. Drift
// between them is caught in smoke_nginx_test.go —
// TestValidApplyRunsStatusInSyncWithKeeper.
//
// ADR-039(4): fail-early at test startup if an expectation names an invalid
// status (a typo like "succeeded" / "done" is caught immediately, not at the
// assert phase).
var validApplyRunsStatus = map[string]struct{}{
	"planned":    {},
	"claimed":    {},
	"running":    {},
	"dispatched": {},
	"success":    {},
	"failed":     {},
	"cancelled":  {},
	"orphaned":   {},
	"no_match":   {},
}

// CheckApplyRunsStatusValid validates that a string from a YAML expectation is
// an allowed apply_runs.status enum value; fails t if not. Does not assert
// against the DB (use AssertApplyRunsStatus for that).
func CheckApplyRunsStatusValid(t *testing.T, status string) {
	t.Helper()
	if !IsValidApplyRunsStatus(status) {
		known := ValidApplyRunsStatuses()
		t.Fatalf("unknown apply_runs.status value %q in expectations; allowed: %v", status, known)
	}
}

// IsValidApplyRunsStatus — pure check without testing.TB. Used by the drift
// tests in smoke_nginx_test.go, which need a boolean result without failing
// the current test as a side effect.
func IsValidApplyRunsStatus(status string) bool {
	_, ok := validApplyRunsStatus[status]
	return ok
}

// ValidApplyRunsStatuses returns a copy of the valid-values map for the drift
// test (smoke_nginx_test.go::TestValidApplyRunsStatusInSyncWithKeeper).
// Returns a slice, not a map (ordered comparison is simpler).
func ValidApplyRunsStatuses() []string {
	out := make([]string, 0, len(validApplyRunsStatus))
	for k := range validApplyRunsStatus {
		out = append(out, k)
	}
	return out
}

// AssertApplyRunsStatus reads apply_runs rows by applyID from PG and fails if
// any of them does not equal expected.
//
// PK apply_runs = (apply_id, sid) → one run yields N rows (one per Soul
// host). The harness requires success on ALL of them — a half-applied result
// is not modeled in the MVP expectations (multi-host failures immediately
// leave error_locked).
func (s *Stack) AssertApplyRunsStatus(t *testing.T, applyID string, expected string) {
	t.Helper()
	CheckApplyRunsStatusValid(t, expected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := s.db.Query(ctx,
		"SELECT sid, status FROM apply_runs WHERE apply_id = $1", applyID)
	if err != nil {
		t.Fatalf("AssertApplyRunsStatus %s: query: %v", applyID, err)
	}
	defer rows.Close()

	statuses := map[string]string{}
	for rows.Next() {
		var sid, st string
		if err := rows.Scan(&sid, &st); err != nil {
			t.Fatalf("AssertApplyRunsStatus %s: scan: %v", applyID, err)
		}
		statuses[sid] = st
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertApplyRunsStatus %s: rows.Err: %v", applyID, err)
	}
	if len(statuses) == 0 {
		t.Fatalf("AssertApplyRunsStatus %s: no apply_runs rows", applyID)
	}
	for sid, st := range statuses {
		if st != expected {
			t.Fatalf("AssertApplyRunsStatus %s: sid=%s status=%q, expected %q (full matrix=%v)",
				applyID, sid, st, expected, statuses)
		}
	}
}

// AssertIncarnationState reads incarnation.state from the DB and fails if the
// jsonb payload does not contain expectedSubset (deep-subset comparison).
func (s *Stack) AssertIncarnationState(t *testing.T, name string, expectedSubset map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stateJSON []byte
	err := s.db.QueryRow(ctx,
		"SELECT state FROM incarnation WHERE name = $1", name).Scan(&stateJSON)
	if err != nil {
		t.Fatalf("AssertIncarnationState %s: query: %v", name, err)
	}
	// state can be NULL (a new incarnation without an apply) — expectations
	// won't match at this stage either way, so emit clear diagnostics.
	if len(stateJSON) == 0 || string(stateJSON) == "null" {
		t.Fatalf("AssertIncarnationState %s: state is empty, expected subset=%v", name, expectedSubset)
	}
	var actual map[string]any
	if err := json.Unmarshal(stateJSON, &actual); err != nil {
		t.Fatalf("AssertIncarnationState %s: unmarshal state: %v (raw=%s)", name, err, string(stateJSON))
	}
	if !subsetMatches(actual, expectedSubset) {
		t.Fatalf("AssertIncarnationState %s: state does not contain subset\nactual=%v\nexpected_subset=%v",
			name, actual, expectedSubset)
	}
}

// AssertAuditEvent looks in audit_log for at least one row with
// event_type=eventType and a payload containing the expectedPayload subset.
// Fails if none found.
//
// Implemented via the jsonb `@>` operator (subset match), equivalent to a
// deep-subset comparison on the Go side. No ARRAY_CONTAINS — payload subset of
// the declared fields only.
func (s *Stack) AssertAuditEvent(t *testing.T, eventType string, expectedPayload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subsetJSON, err := json.Marshal(expectedPayload)
	if err != nil {
		t.Fatalf("AssertAuditEvent: marshal expected payload: %v", err)
	}

	var count int
	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload @> $2::jsonb
	`, eventType, string(subsetJSON)).Scan(&count)
	if err != nil {
		t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
	}
	if count == 0 {
		// Dump all events of type eventType for diagnostics (at most 10 rows).
		rows, derr := s.db.Query(ctx,
			"SELECT payload FROM audit_log WHERE event_type = $1 ORDER BY created_at DESC LIMIT 10",
			eventType)
		var dumps []string
		if derr == nil {
			defer rows.Close()
			for rows.Next() {
				var p []byte
				if err := rows.Scan(&p); err == nil {
					dumps = append(dumps, string(p))
				}
			}
		}
		t.Fatalf("AssertAuditEvent %s: payload subset not found\nexpected=%s\nrecent_events=%v",
			eventType, string(subsetJSON), dumps)
	}
}

// AssertMetricGE scrapes the Keeper's /metrics (a separate listener,
// Stack.MetricsURL) and checks that the `metric` value >= a minimum.
//
// metric — a Prometheus expression from expectations.yaml, e.g.
// `keeper_scenario_runs_total{result="ok"}` or a bare name `keeper_xxx_total`.
// Only summing across matching rows is supported (counter/gauge); histogram
// decomposition is not needed in the MVP.
func (s *Stack) AssertMetricGE(t *testing.T, metric string, minimum float64) {
	t.Helper()
	if s.MetricsURL == "" {
		t.Fatal("AssertMetricGE: Stack.MetricsURL is empty (NewStack didn't run?)")
	}
	resp, err := http.Get(s.MetricsURL + "/metrics")
	if err != nil {
		t.Fatalf("AssertMetricGE %s: scrape: %v", metric, err)
	}
	defer resp.Body.Close()

	body, err := readAllLimited(resp.Body, 8*1024*1024)
	if err != nil {
		t.Fatalf("AssertMetricGE %s: read body: %v", metric, err)
	}
	actual, found := parsePrometheusSum(body, metric)
	if !found {
		t.Fatalf("AssertMetricGE %s: metric not found in /metrics", metric)
	}
	if actual < minimum {
		t.Fatalf("AssertMetricGE %s = %v, expected >= %v", metric, actual, minimum)
	}
}

// readAllLimited reads the body up to limitBytes. Protects against
// accidentally scraping a huge /metrics in a test environment.
func readAllLimited(r interface{ Read(p []byte) (int, error) }, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		if int64(buf.Len()) > limit {
			return nil, fmt.Errorf("body exceeds %d bytes", limit)
		}
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			// Not importing io.EOF — comparing the name from the net/http body reader.
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// parsePrometheusSum sums the values of all exposition-format lines whose
// metric name equals bareName(query) and whose label set contains all
// label=value pairs from query.
//
// query grammar (simplified):
//
//	<bare>                              — bare counter/gauge.
//	<bare>{label1="v1",label2="v2"}     — with a label filter.
//
// Returns (sum, true) if at least one line matched. Ignores `# HELP` /
// `# TYPE` lines and lines whose label set doesn't contain all filters (but
// Prometheus convention allows extra labels — this is not a strict match but
// a subset, like jsonb `@>`).
func parsePrometheusSum(body []byte, query string) (float64, bool) {
	name, filters, ok := parseQuery(query)
	if !ok {
		return 0, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var sum float64
	matched := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		ln, lbls, val, ok := parseExpositionLine(line)
		if !ok {
			continue
		}
		if ln != name {
			continue
		}
		if !labelsContain(lbls, filters) {
			continue
		}
		sum += val
		matched = true
	}
	return sum, matched
}

// parseQuery splits `name{...}` into (name, label-filters).
func parseQuery(q string) (name string, filters map[string]string, ok bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil, false
	}
	br := strings.IndexByte(q, '{')
	if br < 0 {
		return q, nil, true
	}
	if !strings.HasSuffix(q, "}") {
		return "", nil, false
	}
	name = strings.TrimSpace(q[:br])
	inner := q[br+1 : len(q)-1]
	filters = map[string]string{}
	if strings.TrimSpace(inner) == "" {
		return name, filters, true
	}
	// A simple parser for `k="v",k2="v2"`. Doesn't handle escapes — good enough for the MVP.
	for _, part := range splitTopLevelCommas(inner) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return "", nil, false
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		v = strings.Trim(v, `"`)
		filters[k] = v
	}
	return name, filters, true
}

// splitTopLevelCommas splits on commas outside of quotes.
func splitTopLevelCommas(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			inQ = !inQ
			cur.WriteByte(c)
		case ',':
			if inQ {
				cur.WriteByte(c)
			} else {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseExpositionLine parses a Prometheus exposition format line:
//
//	<name>[{label1="v1",label2="v2"}] <value> [timestamp]
//
// Returns (name, labels, value, ok). Ignores a trailing timestamp.
func parseExpositionLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	// Split on the last space: `<metric-block> <value> [ts]`.
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return "", nil, 0, false
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+1:])

	// A trailing timestamp is possible: `name 1.0 1717000000000`. Reparse if
	// so: if right isn't a number, shift the split.
	if v, err := strconv.ParseFloat(right, 64); err == nil {
		// The direct case: all good, value=right.
		_ = v
	} else {
		// right is not a number — might be a timestamp; try shifting the split.
		idx2 := strings.LastIndex(left, " ")
		if idx2 < 0 {
			return "", nil, 0, false
		}
		newRight := strings.TrimSpace(left[idx2+1:])
		left = strings.TrimSpace(left[:idx2])
		right = newRight
	}

	value, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return "", nil, 0, false
	}

	// left = `name` or `name{labels}`.
	br := strings.IndexByte(left, '{')
	if br < 0 {
		return left, nil, value, true
	}
	if !strings.HasSuffix(left, "}") {
		return "", nil, 0, false
	}
	name = left[:br]
	inner := left[br+1 : len(left)-1]
	labels = map[string]string{}
	for _, part := range splitTopLevelCommas(inner) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		v = strings.Trim(v, `"`)
		labels[k] = v
	}
	return name, labels, value, true
}

// labelsContain — every pair in want is present in have with the same value.
func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// subsetMatches — recursive deep-subset check for map[string]any. Supports:
//
//   - map[string]any (recursively);
//   - []any (exact ordered equality via reflect.DeepEqual);
//   - primitives (via reflect.DeepEqual + EqualNumbers normalization
//     int<->float64, since JSON-decoded numbers are always float64 while
//     callers often write int literals).
func subsetMatches(actual, expected map[string]any) bool {
	for k, ev := range expected {
		av, ok := actual[k]
		if !ok {
			return false
		}
		if !valueMatches(av, ev) {
			return false
		}
	}
	return true
}

func valueMatches(actual, expected any) bool {
	// If expected is a map, recurse.
	if em, ok := expected.(map[string]any); ok {
		am, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		return subsetMatches(am, em)
	}
	// Number normalization.
	if af, ok := toFloat(actual); ok {
		if ef, ok := toFloat(expected); ok {
			return af == ef
		}
	}
	// Ordered slice comparison — DeepEqual after number normalization.
	if es, ok := expected.([]any); ok {
		as, ok := actual.([]any)
		if !ok || len(as) != len(es) {
			return false
		}
		for i := range es {
			if !valueMatches(as[i], es[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(actual, expected)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	default:
		return 0, false
	}
}

// sortedKeys — helper for stable diagnostic messages.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = sortedKeys // reserved for diagnostics of future asserts
