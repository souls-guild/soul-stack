//go:build e2e_live

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

// validApplyRunsStatus — closed set of apply_runs.status values allowed to
// the harness in YAML expectations.
//
// Source of truth — keeper/internal/applyrun/applyrun.go (Status consts +
// ValidStatus). Duplicated here as a literal because tests/e2e-live is a
// separate go module without a dependency on keeper/ (testcontainers deps
// used by the pilot must not leak into the main modules). Drift between them
// is caught by the L3a/L3b smoke tests — TestValidApplyRunsStatusInSyncWithKeeper.
//
// ADR-039(4): fail-early at test start if an expectation specifies an invalid
// status (a typo like "succeeded" / "done" is visible immediately, not at
// assert time).
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

// CheckApplyRunsStatusValid validates that a value from a YAML expectation
// is an allowed apply_runs.status enum value; fails t if not. Does not run
// an assert against the DB (for that, AssertApplyRunsStatus).
func CheckApplyRunsStatusValid(t *testing.T, status string) {
	t.Helper()
	if !IsValidApplyRunsStatus(status) {
		known := ValidApplyRunsStatuses()
		t.Fatalf("unknown apply_runs.status value %q in expectations; allowed: %v", status, known)
	}
}

// IsValidApplyRunsStatus — pure check without testing.TB.
func IsValidApplyRunsStatus(status string) bool {
	_, ok := validApplyRunsStatus[status]
	return ok
}

// ValidApplyRunsStatuses returns a copy of the valid-values map for the
// drift test.
func ValidApplyRunsStatuses() []string {
	out := make([]string, 0, len(validApplyRunsStatus))
	for k := range validApplyRunsStatus {
		out = append(out, k)
	}
	return out
}

// AssertApplyRunsStatus reads apply_runs rows by applyID from PG and fails
// if any of them is not equal to expected.
//
// PK apply_runs = (apply_id, sid) → one run gives N rows (one per Soul
// host). The harness requires success for ALL — half-applied results aren't
// modeled in MVP expectations (multi-host failures leave error_locked
// immediately).
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

// AssertApplyHostStatus checks the apply_runs status of a SPECIFIC host (sid) —
// unlike AssertApplyRunsStatus, which requires a single status across ALL
// rows. Needed for runs with per-host where: targeting ONE host (split-brain
// guard, failed_when fail-stop): the target host = failed, other roster hosts
// that ended up with 0 tasks after on:/where: = no_match (not failed).
func (s *Stack) AssertApplyHostStatus(t *testing.T, applyID, sid, expected string) {
	t.Helper()
	CheckApplyRunsStatusValid(t, expected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRow(ctx,
		"SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2", applyID, sid).Scan(&status)
	if err != nil {
		t.Fatalf("AssertApplyHostStatus(apply=%s sid=%s): no apply_runs row: %v", applyID, sid, err)
	}
	if status != expected {
		t.Fatalf("AssertApplyHostStatus(apply=%s sid=%s): status=%q, expected %q", applyID, sid, status, expected)
	}
}

// AssertIncarnationState reads incarnation.state from the DB and fails if
// the jsonb payload does not contain expectedSubset (deep-subset comparison).
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
func (s *Stack) AssertAuditEvent(t *testing.T, eventType string, expectedPayload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Empty/nil payload (fixture `audit_events: [{type: ...}]` without a
	// payload) — presence check by event_type only. `payload @> 'null'::jsonb`
	// wouldn't match an object payload and would give a false fail; for
	// "an event of this type exists" no payload filter is needed.
	var (
		count int
		err   error
	)
	if len(expectedPayload) == 0 {
		err = s.db.QueryRow(ctx,
			"SELECT COUNT(*) FROM audit_log WHERE event_type = $1", eventType).Scan(&count)
		if err != nil {
			t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
		}
		if count == 0 {
			t.Fatalf("AssertAuditEvent %s: no event of this type", eventType)
		}
		return
	}

	subsetJSON, err := json.Marshal(expectedPayload)
	if err != nil {
		t.Fatalf("AssertAuditEvent: marshal expected payload: %v", err)
	}

	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload @> $2::jsonb
	`, eventType, string(subsetJSON)).Scan(&count)
	if err != nil {
		t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
	}
	if count == 0 {
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

// AssertMetricGE scrapes Keeper's /metrics (separate listener,
// Stack.MetricsURL) and checks that the `metric` value is >= minimum.
func (s *Stack) AssertMetricGE(t *testing.T, metric string, minimum float64) {
	t.Helper()
	if s.MetricsURL == "" {
		t.Fatal("AssertMetricGE: Stack.MetricsURL is empty (NewStack did not run?)")
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
// accidentally scraping a giant /metrics in the test environment.
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
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

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

func parseExpositionLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return "", nil, 0, false
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+1:])

	if v, err := strconv.ParseFloat(right, 64); err == nil {
		_ = v
	} else {
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

func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

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
	if em, ok := expected.(map[string]any); ok {
		am, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		return subsetMatches(am, em)
	}
	if af, ok := toFloat(actual); ok {
		if ef, ok := toFloat(expected); ok {
			return af == ef
		}
	}
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

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = sortedKeys // reserved for diagnostics of future asserts

// Container-side asserts (L3b-4 implementation).
//
// All four asserts run commands inside a privileged Debian-12 systemd-soul
// container via SoulContainer.Exec. SoulContainer.Exec returns combined
// stdout+stderr (testcontainers-go multiplexed reader), so the harness does
// not split the streams: the exit code is enough for the assert, the body is
// only used in diag messages.
//
// hostExecTimeout — the upper cap on one Exec. Commands are cheap (dpkg-query,
// systemctl is-active, stat, cat | grep) → 30s with margin; on slow CI
// systemd can take up to 1-2s even for is-active.
const hostExecTimeout = 30 * time.Second

// soulContainerByIdx returns a SoulContainer by index or fails t with a
// diagnostic out-of-range message. Internal helper for AssertHost*.
func (s *Stack) soulContainerByIdx(t *testing.T, soulIdx int) *SoulContainer {
	t.Helper()
	if soulIdx < 0 || soulIdx >= len(s.SoulContainers) {
		t.Fatalf("soulIdx %d out of range (have %d soul containers)",
			soulIdx, len(s.SoulContainers))
	}
	sc := s.SoulContainers[soulIdx]
	if sc == nil {
		t.Fatalf("SoulContainers[%d] = nil", soulIdx)
	}
	return sc
}

// AssertHostPkgInstalled checks that a Debian package is actually installed
// in the soul container. Via `dpkg-query -W -f=${Status} <pkg>`: a status
// string of `install ok installed` is the only valid value for "fully
// installed" (there are intermediate states: `deinstall ok config-files`,
// `purge ok not-installed`, etc). The container is always debian-12 (see
// container.go), no rpm branch needed.
func (s *Stack) AssertHostPkgInstalled(t *testing.T, soulIdx int, pkg string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"dpkg-query", "-W", "-f=${Status}", pkg})
	if err != nil {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): exec dpkg-query: %v\noutput=%s",
			soulIdx, pkg, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): dpkg-query exit=%d\noutput=%s",
			soulIdx, pkg, code, out)
	}
	status := strings.TrimSpace(out)
	if !strings.Contains(status, "install ok installed") {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): package not installed correctly, status=%q",
			soulIdx, pkg, status)
	}
}

// AssertHostServiceActive checks that a systemd unit is active via
// `systemctl is-active <svc>`. is-active returns exit=0 if active, otherwise
// nonzero (3 for inactive/failed/unknown). At assert time what matters is
// the textual stdout value (`active`), the exit code is just a duplicate.
func (s *Stack) AssertHostServiceActive(t *testing.T, soulIdx int, svc string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"systemctl", "is-active", svc})
	if err != nil {
		t.Fatalf("AssertHostServiceActive(soulIdx=%d svc=%s): exec systemctl: %v\noutput=%s",
			soulIdx, svc, err, out)
	}
	status := strings.TrimSpace(out)
	if status != "active" {
		t.Fatalf("AssertHostServiceActive(soulIdx=%d svc=%s): status=%q (exit=%d), expected 'active'",
			soulIdx, svc, status, code)
	}
}

// AssertHostFileExists checks that a file/directory at path exists inside
// the soul container. Uses `stat -c %F <path>` — exit=0 and non-empty stdout
// (object type: `regular file`/`directory`/…). Existence only, no type check:
// for type/perm checks the caller should invoke additional asserts (not yet
// introduced by L3b-4).
func (s *Stack) AssertHostFileExists(t *testing.T, soulIdx int, path string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"stat", "-c", "%F", path})
	if err != nil {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): exec stat: %v\noutput=%s",
			soulIdx, path, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): stat exit=%d\noutput=%s",
			soulIdx, path, code, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): stat returned an empty result",
			soulIdx, path)
	}
}

// AssertHostFileContent checks that a file at path contains substring substr.
// Command: `cat <path> | grep -F -- <substr>`; grep exit=0 — substring found,
// 1 — not found, >=2 — error. Shell arguments are passed via single-quote
// escaping (shellQuote); arbitrary user input in test fixtures is not expected.
func (s *Stack) AssertHostFileContent(t *testing.T, soulIdx int, path, substr string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	script := fmt.Sprintf("cat %s | grep -F -- %s", shellQuote(path), shellQuote(substr))
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertHostFileContent(soulIdx=%d path=%s substr=%q): exec: %v\noutput=%s",
			soulIdx, path, substr, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostFileContent(soulIdx=%d path=%s substr=%q): substring not found (grep exit=%d)\noutput=%s",
			soulIdx, path, substr, code, out)
	}
}

// AssertHostHTTPContains does an HTTP GET on url INSIDE the soul container
// (curl, present in the L3b Dockerfile) and checks that the response body
// contains substr. Polls for up to retrySec seconds: the network service
// (node_exporter :9100/metrics) comes up asynchronously after systemctl
// start — the exporter needs a second to bind the listen socket.
//
// This is the piggyback check for node-exporter: url=http://127.0.0.1:9100/metrics,
// substr="node_" (any node_exporter metric) confirms that the binary is
// deployed, the systemd unit is active AND the port is actually listening
// and serving /metrics — which the services/files checks alone don't prove.
//
// curl -fsS: -f → nonzero exit on HTTP >= 400, -s → no progress bar, -S →
// show the error. exit 0 + substr in the body = success.
func (s *Stack) AssertHostHTTPContains(t *testing.T, soulIdx int, url, substr string, retrySec int) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	var lastOut string
	var lastCode int
	deadline := time.Now().Add(time.Duration(retrySec) * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
		script := fmt.Sprintf("curl -fsS %s | grep -F -- %s", shellQuote(url), shellQuote(substr))
		out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
		cancel()
		if err != nil {
			t.Fatalf("AssertHostHTTPContains(soulIdx=%d url=%s): exec: %v\noutput=%s",
				soulIdx, url, err, out)
		}
		lastOut, lastCode = out, code
		if code == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("AssertHostHTTPContains(soulIdx=%d url=%s substr=%q): not received within %ds (curl|grep exit=%d)\noutput=%s",
		soulIdx, url, substr, retrySec, lastCode, lastOut)
}

// AssertRedisACLUser checks that a user is visible in the LIVE redis via
// redis-cli ACL LIST (AUTH admin) — a live effect of community.redis.acl,
// not just state.
func (s *Stack) AssertRedisACLUser(t *testing.T, soulIdx int, host string, port int, adminUser, adminPass, wantUser string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	// --no-auth-warning silences the stderr warning about a password in argv
	// (Exec merges stdout+stderr). `^user <name> ` — a clean boundary (doesn't
	// match a same-prefix neighbor).
	script := fmt.Sprintf("redis-cli -h %s -p %d --user %s --pass %s --no-auth-warning ACL LIST | grep -E %s",
		shellQuote(host), port, shellQuote(adminUser), shellQuote(adminPass), shellQuote("^user "+wantUser+" "))
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisACLUser(soulIdx=%d user=%s): exec: %v\noutput=%s", soulIdx, wantUser, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisACLUser(soulIdx=%d user=%s): not in the live ACL LIST (redis-cli|grep exit=%d) — did community.redis.acl fail to apply the user?\noutput=%s",
			soulIdx, wantUser, code, out)
	}
}

// shellQuote wraps a string in single quotes, escaping internal single
// quotes per the POSIX `'\"` pattern. Used only for paths and substrings
// from test fixtures (controlled input, not user data).
func shellQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// ── Redis day-2 asserts (NIM-54, L3b) ───────────────────────────────────────
//
// Live redis-cli/openssl asserts for day-2 scenarios (update_config / restart /
// rotate_tls / update_users) on top of the soul container. A single connection
// descriptor RedisConn (plain XOR server-only-TLS) + a private redisCLIPrefix
// builder — all public asserts are built on top of it with a single sc.Exec.
// redis-cli is present after the create scenario installs redis-server (same
// as AssertRedisACLUser); openssl is in the L3b Dockerfile (needed by
// AssertRedisTLSCertServed).

// defaultRedisCACertPath — default ca.crt path inside the soul container for
// TLS (essence.conf_dir=/etc/redis, the PEM is rendered by core.file.present
// into ${conf_dir}/tls).
const defaultRedisCACertPath = "/etc/redis/tls/ca.crt"

// RedisConn — redis-cli connection parameters from the soul container (plain or TLS).
type RedisConn struct {
	SoulIdx    int
	Host       string
	Port       int
	User       string
	Pass       string
	TLS        bool
	CACertPath string // ca.crt inside the container for TLS; empty → defaultRedisCACertPath
}

// redisCLIPrefix builds the `redis-cli …` prefix from RedisConn: server-only
// TLS (--tls --cacert) when TLS is on, authentication (--user/--pass), -h/-p
// and --no-auth-warning (silences the stderr password-in-argv warning — Exec
// merges stdout+stderr). Values are shell-quoted; the caller appends the
// redis command.
func (c RedisConn) redisCLIPrefix() string {
	var b strings.Builder
	b.WriteString("redis-cli --no-auth-warning")
	if c.TLS {
		ca := c.CACertPath
		if ca == "" {
			ca = defaultRedisCACertPath
		}
		b.WriteString(" --tls --cacert " + shellQuote(ca))
	}
	b.WriteString(" -h " + shellQuote(c.Host))
	fmt.Fprintf(&b, " -p %d", c.Port)
	if c.User != "" {
		b.WriteString(" --user " + shellQuote(c.User))
	}
	if c.Pass != "" {
		b.WriteString(" --pass " + shellQuote(c.Pass))
	}
	return b.String()
}

// nonEmptyLines splits redis-cli combined output into lines, trims
// whitespace/CR (redis INFO uses CRLF), and drops empty ones.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// AssertRedisConfigGet checks the live value of `CONFIG GET <param>` == want
// (maxmemory / maxmemory-policy / appendonly, etc). redis-cli in raw mode
// (non-TTY pipe) returns two lines — parameter name and value; we compare the second.
func (s *Stack) AssertRedisConfigGet(t *testing.T, c RedisConn, param, want string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, c.SoulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	script := c.redisCLIPrefix() + " CONFIG GET " + shellQuote(param)
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisConfigGet(soulIdx=%d param=%s): exec: %v\noutput=%s", c.SoulIdx, param, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisConfigGet(soulIdx=%d param=%s): redis-cli exit=%d\noutput=%s", c.SoulIdx, param, code, out)
	}
	lines := nonEmptyLines(out)
	if len(lines) < 2 {
		t.Fatalf("AssertRedisConfigGet(soulIdx=%d param=%s): CONFIG GET returned <2 lines (parameter unknown to redis?)\noutput=%q", c.SoulIdx, param, out)
	}
	if got := lines[1]; got != want {
		t.Fatalf("AssertRedisConfigGet(soulIdx=%d param=%s): value=%q, expected %q\noutput=%q", c.SoulIdx, param, got, want, out)
	}
}

// AssertRedisConfFileDirective reads redis.conf in the container (cat <confPath>)
// and checks for a line `<directive> <want>`. For startup-only directives
// (io-threads etc — community.redis startupOnlyDirectives), which are only
// visible ON DISK, not through CONFIG GET. confPath is usually /etc/redis/redis.conf.
func (s *Stack) AssertRedisConfFileDirective(t *testing.T, soulIdx int, confPath, directive, want string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"cat", confPath})
	if err != nil {
		t.Fatalf("AssertRedisConfFileDirective(soulIdx=%d conf=%s directive=%s): cat: %v\noutput=%s", soulIdx, confPath, directive, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisConfFileDirective(soulIdx=%d conf=%s directive=%s): cat exit=%d (file missing?)\noutput=%s", soulIdx, confPath, directive, code, out)
	}
	for _, ln := range nonEmptyLines(out) {
		if strings.HasPrefix(ln, "#") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 2 || fields[0] != directive {
			continue
		}
		if strings.Join(fields[1:], " ") == want {
			return
		}
	}
	t.Fatalf("AssertRedisConfFileDirective(soulIdx=%d conf=%s): directive `%s %s` not found on disk", soulIdx, confPath, directive, want)
}

// redisInfoField runs `INFO <section>` and returns the value of field
// `<field>:<value>` (redis INFO — CRLF-separated `k:v`). Fails t if redis-cli
// crashed or the field is missing. Shared bottom for AssertRedisRole/UptimeBelow.
func (s *Stack) redisInfoField(t *testing.T, c RedisConn, section, field string) string {
	t.Helper()
	sc := s.soulContainerByIdx(t, c.SoulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	script := c.redisCLIPrefix() + " INFO " + shellQuote(section)
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("redisInfoField(soulIdx=%d section=%s field=%s): exec: %v\noutput=%s", c.SoulIdx, section, field, err, out)
	}
	if code != 0 {
		t.Fatalf("redisInfoField(soulIdx=%d section=%s field=%s): redis-cli exit=%d\noutput=%s", c.SoulIdx, section, field, code, out)
	}
	prefix := field + ":"
	for _, ln := range nonEmptyLines(out) {
		if strings.HasPrefix(ln, prefix) {
			return strings.TrimPrefix(ln, prefix)
		}
	}
	t.Fatalf("redisInfoField(soulIdx=%d section=%s): field %q not found\noutput=%q", c.SoulIdx, section, field, out)
	return ""
}

// AssertRedisRole checks the node's role (`INFO replication` → role:) == wantRole
// ("master"/"slave"). For restart/failover scenarios.
func (s *Stack) AssertRedisRole(t *testing.T, c RedisConn, wantRole string) {
	t.Helper()
	if got := s.redisInfoField(t, c, "replication", "role"); got != wantRole {
		t.Fatalf("AssertRedisRole(soulIdx=%d): role=%q, expected %q", c.SoulIdx, got, wantRole)
	}
}

// AssertRedisUptimeBelow checks `INFO server` uptime_in_seconds < maxSec —
// proof of a restart (uptime reset). For the restart scenario.
func (s *Stack) AssertRedisUptimeBelow(t *testing.T, c RedisConn, maxSec int) {
	t.Helper()
	raw := s.redisInfoField(t, c, "server", "uptime_in_seconds")
	uptime, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("AssertRedisUptimeBelow(soulIdx=%d): uptime_in_seconds=%q not a number: %v", c.SoulIdx, raw, err)
	}
	if uptime >= maxSec {
		t.Fatalf("AssertRedisUptimeBelow(soulIdx=%d): uptime=%ds >= %ds — restart did not happen?", c.SoulIdx, uptime, maxSec)
	}
}

// AssertRedisACLUserAbsent checks that a user is NOT in the live `ACL LIST`
// (inverse of AssertRedisACLUser) — for update_users, where the old user was removed.
func (s *Stack) AssertRedisACLUserAbsent(t *testing.T, c RedisConn, user string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, c.SoulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	// No pipe into grep: first make sure redis-cli succeeded (exit 0), then
	// check the lines — otherwise "grep found nothing" is indistinguishable
	// from "redis-cli crashed".
	script := c.redisCLIPrefix() + " ACL LIST"
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisACLUserAbsent(soulIdx=%d user=%s): exec: %v\noutput=%s", c.SoulIdx, user, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisACLUserAbsent(soulIdx=%d user=%s): redis-cli ACL LIST exit=%d\noutput=%s", c.SoulIdx, user, code, out)
	}
	prefix := "user " + user + " "
	for _, ln := range nonEmptyLines(out) {
		if strings.HasPrefix(ln, prefix) {
			t.Fatalf("AssertRedisACLUserAbsent(soulIdx=%d user=%s): user still in ACL LIST (not removed?)\nline=%q", c.SoulIdx, user, ln)
		}
	}
}

// AssertRedisACLUserPerms checks that `ACL GETUSER <user>` contains the
// permissions substring wantPermsSubstr (e.g. "+@read") — for update_users,
// where the user was assigned a new permission set. A missing user → `(nil)`,
// substring not found → fail.
func (s *Stack) AssertRedisACLUserPerms(t *testing.T, c RedisConn, user, wantPermsSubstr string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, c.SoulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	script := c.redisCLIPrefix() + " ACL GETUSER " + shellQuote(user)
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisACLUserPerms(soulIdx=%d user=%s): exec: %v\noutput=%s", c.SoulIdx, user, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisACLUserPerms(soulIdx=%d user=%s): redis-cli ACL GETUSER exit=%d\noutput=%s", c.SoulIdx, user, code, out)
	}
	if !strings.Contains(out, wantPermsSubstr) {
		t.Fatalf("AssertRedisACLUserPerms(soulIdx=%d user=%s): permissions %q not found (user missing / different permission set?)\noutput=%s", c.SoulIdx, user, wantPermsSubstr, out)
	}
}

// AssertRedisTLSCertServed extracts the SHA-256 fingerprint of the server
// cert that redis actually SERVES over TLS, and compares it against the
// expected one. For rotate_tls (fingerprint BEFORE/AFTER rotation proves the
// LIVE cert changed, not just the file on disk). Implementation — openssl
// s_client (redis server-only-TLS, tls-auth-clients no → client cert not
// needed) | openssl x509 -fingerprint. openssl is present in the L3b
// Dockerfile. Case/colons are normalized.
func (s *Stack) AssertRedisTLSCertServed(t *testing.T, c RedisConn, wantFingerprintSHA256 string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, c.SoulIdx)

	want := normalizeHexFingerprint(wantFingerprintSHA256)
	if want == "" {
		t.Fatalf("AssertRedisTLSCertServed(soulIdx=%d): empty expected fingerprint", c.SoulIdx)
	}

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s:%d", c.Host, c.Port)
	script := fmt.Sprintf(
		"openssl s_client -connect %s </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256",
		shellQuote(endpoint))
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisTLSCertServed(soulIdx=%d %s): exec: %v\noutput=%s", c.SoulIdx, endpoint, err, out)
	}
	got := normalizeHexFingerprint(out)
	if code != 0 || got == "" {
		t.Fatalf("AssertRedisTLSCertServed(soulIdx=%d %s): openssl did not return a fingerprint (redis not listening on TLS / no openssl? exit=%d)\noutput=%s", c.SoulIdx, endpoint, code, out)
	}
	if got != want {
		t.Fatalf("AssertRedisTLSCertServed(soulIdx=%d %s): served fingerprint=%s, expected %s", c.SoulIdx, endpoint, got, want)
	}
}

// normalizeHexFingerprint extracts the hex part of the fingerprint (after
// '=' if present — form `sha256 Fingerprint=AA:BB:…`) and keeps only hex
// digits in upper case: "AA:BB", "aabb", the openssl string are all reduced
// to one form.
func normalizeHexFingerprint(s string) string {
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			b.WriteByte(ch)
		}
	}
	return strings.ToUpper(b.String())
}

// ── Per-task flow-control asserts (FC-0) ────────────────────────────────────
//
// ★ FC-0 EXPLORATION. Per-task TaskStatus (SKIPPED/OK/CHANGED/FAILED/TIMED_OUT/
// CANCELLED) and error.code (flowcontrol.failed_when / flowcontrol.until_exhausted
// / …) are NOT persisted by keeper in a separate task table and are NOT
// stored as a column in apply_runs. The only per-task persistence surface is
// audit_log:
//
//   keeper/internal/grpc/events_taskevent.go → handleTaskEvent → AuditWriter.Write(
//       EventType = "task.executed", Source = "soul_grpc",
//       CorrelationID = apply_id,
//       Payload = shared/audit.BuildTaskExecutedPayload{...})
//
// Every task (including SKIPPED — soul emits skippedTaskEvent,
// applyrunner.go) produces one audit_log row. The payload shape is fixed by
// shared/audit.BuildTaskExecutedPayload:
//
//   {sid, apply_id, task_idx, plan_index, status, passage,
//    error?:{code, module, message?}, register_data?, suppressed?}
//
// where status = keeperv1.TaskStatus.String() (literal "TASK_STATUS_SKIPPED"
// etc), error.code = TaskError.code (for no_log errors the message is
// omitted but code and module are kept). Hence per-task FC asserts read
// audit_log, NOT apply_runs.
//
// CORRELATION KEY — plan_index (GLOBAL end-to-end task index across the
// whole plan, migrations 079/081, ADR-056 §S1). The local task_idx is not
// unique across Passage or across hosts within the same Passage (per-host
// where:), so per-task asserts key on plan_index. N=1 run → plan_index == task_idx.

// taskStatusLiteralByEnum — closed set of keeperv1.TaskStatus.String()
// string literals that the audit payload carries in payload->>'status'.
// Source of truth — proto/keeper/v1/apply.proto (enum TaskStatus). Duplicated
// here as a literal: tests/e2e-live is a separate go module without a
// dependency on proto/gen. Fail-early on a typo in an expectation
// (ADR-039(4), same as validApplyRunsStatus).
var taskStatusLiteralByEnum = map[string]struct{}{
	"TASK_STATUS_UNSPECIFIED": {},
	"TASK_STATUS_OK":          {},
	"TASK_STATUS_CHANGED":     {},
	"TASK_STATUS_FAILED":      {},
	"TASK_STATUS_TIMED_OUT":   {},
	"TASK_STATUS_SKIPPED":     {},
	"TASK_STATUS_CANCELLED":   {},
}

// IsValidTaskStatus — pure check of a per-task status string literal.
func IsValidTaskStatus(status string) bool {
	_, ok := taskStatusLiteralByEnum[status]
	return ok
}

// AssertTaskStatus checks that the per-task status of a task (apply_id, sid,
// plan_index, passage) equals wantStatus. Reads audit_log (event_type=
// task.executed, correlation_id=apply_id), not apply_runs — keeper persists
// per-task TaskStatus ONLY in audit (see the block doc comment above).
//
// planIdx — the task's GLOBAL end-to-end plan_index (not the local task_idx);
// for an N=1 run it matches the task's position in the plan. passage — the
// staged-render Passage index (0 = the only one).
//
// wantStatus — a keeperv1.TaskStatus.String() string literal, e.g.
// "TASK_STATUS_SKIPPED" / "TASK_STATUS_FAILED" / "TASK_STATUS_CHANGED". An
// invalid literal in an expectation → fail-early (typo visible immediately).
//
// We take the LATEST row by created_at: a retry of the same task emits a
// TaskEvent for the last attempt (one TaskEvent per task — see
// runTaskWithRetry), but a duplicate under cross-Keeper routing is possible;
// "last wins" matches register semantics.
func (s *Stack) AssertTaskStatus(t *testing.T, applyID, sid string, planIdx, passage int, wantStatus string) {
	t.Helper()
	if !IsValidTaskStatus(wantStatus) {
		t.Fatalf("AssertTaskStatus: unknown TaskStatus literal %q (allowed: TASK_STATUS_OK/CHANGED/FAILED/TIMED_OUT/SKIPPED/CANCELLED/UNSPECIFIED)", wantStatus)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRow(ctx, `
		SELECT payload->>'status'
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
		ORDER BY created_at DESC
		LIMIT 1
	`, applyID, sid, planIdx, passage).Scan(&status)
	if err != nil {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskStatus(apply=%s sid=%s plan_index=%d passage=%d): no task.executed row in audit_log (task not executed / TaskEvent did not arrive?): %v",
			applyID, sid, planIdx, passage, err)
	}
	if status != wantStatus {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskStatus(apply=%s sid=%s plan_index=%d passage=%d): status=%q, expected %q",
			applyID, sid, planIdx, passage, status, wantStatus)
	}
}

// AssertTaskErrorCode checks the error.code of a task (apply_id, sid,
// plan_index, passage). Proves the failure class: flowcontrol.failed_when
// (business failure by a CEL predicate) vs flowcontrol.until_exhausted vs a
// module error (e.g. "pkg.not_found"). error.code is persisted in audit_log
// payload->'error'->>'code' (see shared/audit.BuildTaskExecutedPayload);
// apply_runs stores only a composed error_summary TEXT, not a structured code.
//
// For no_log tasks error.message is suppressed, but code and module are
// still stored — this assert works on no_log tasks too.
//
// wantCode — the exact TaskError.code literal, e.g. "flowcontrol.failed_when".
// If the task has no error (OK/CHANGED/SKIPPED) → error.code is missing → fail.
func (s *Stack) AssertTaskErrorCode(t *testing.T, applyID, sid string, planIdx, passage int, wantCode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var code string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(payload->'error'->>'code', '<no-error>')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
		ORDER BY created_at DESC
		LIMIT 1
	`, applyID, sid, planIdx, passage).Scan(&code)
	if err != nil {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskErrorCode(apply=%s sid=%s plan_index=%d passage=%d): no task.executed row in audit_log: %v",
			applyID, sid, planIdx, passage, err)
	}
	if code != wantCode {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskErrorCode(apply=%s sid=%s plan_index=%d passage=%d): error.code=%q, expected %q",
			applyID, sid, planIdx, passage, code, wantCode)
	}
}

// AssertTaskRegisterField reads a single register_data field of a task from
// apply_task_register (PK apply_id, sid, plan_index — migration 079). Returns
// the field's JSON scalar as a string (register_data->>'<field>'); for
// stdout / exit_code / ignored_error / changed / failed we'll need FC-1/FC-4
// (to prove the register of a flow-control task carries the expected value).
//
// register_data — what the REAL soul emitted in TaskEvent.register_data and
// keeper accumulated (accumulateRegister). NOT persisted for a task without
// register: (nil register_data → no row, UpsertTaskRegister is a no-op) →
// the assert will fail with "no row".
//
// planIdx — the GLOBAL end-to-end plan_index (correlation key, not the local
// task_idx). No split by passage: the PK is already unique by plan_index
// across all Passages (the same finding that migration 079 closed).
func (s *Stack) AssertTaskRegisterField(t *testing.T, applyID, sid string, planIdx int, field, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(register_data->>$4, '<null>')
		FROM apply_task_register
		WHERE apply_id = $1 AND sid = $2 AND plan_index = $3
	`, applyID, sid, planIdx, field).Scan(&got)
	if err != nil {
		t.Fatalf("AssertTaskRegisterField(apply=%s sid=%s plan_index=%d field=%s): no register row (task without register:/real soul didn't return a register?): %v",
			applyID, sid, planIdx, field, err)
	}
	if got != want {
		t.Fatalf("AssertTaskRegisterField(apply=%s sid=%s plan_index=%d field=%s): %q, expected %q",
			applyID, sid, planIdx, field, got, want)
	}
}

// dumpTaskEvents prints diagnostics: all task.executed rows for the run on a
// host (plan_index/task_idx/passage/status/error.code). Called on a per-task
// assert failure — without it, "no row" is silent about whether TaskEvents
// arrived at all.
func (s *Stack) dumpTaskEvents(ctx context.Context, t *testing.T, applyID, sid string) {
	t.Helper()
	rows, err := s.db.Query(ctx, `
		SELECT
			COALESCE(payload->>'plan_index','?'),
			COALESCE(payload->>'task_idx','?'),
			COALESCE(payload->>'passage','?'),
			COALESCE(payload->>'status','?'),
			COALESCE(payload->'error'->>'code','-')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1 AND payload->>'sid' = $2
		ORDER BY created_at ASC
	`, applyID, sid)
	if err != nil {
		t.Logf("dumpTaskEvents(apply=%s sid=%s): query: %v", applyID, sid, err)
		return
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var planIdx, taskIdx, passage, status, code string
		if err := rows.Scan(&planIdx, &taskIdx, &passage, &status, &code); err != nil {
			t.Logf("dumpTaskEvents: scan: %v", err)
			return
		}
		lines = append(lines, fmt.Sprintf("plan_index=%s task_idx=%s passage=%s status=%s error_code=%s",
			planIdx, taskIdx, passage, status, code))
	}
	if len(lines) == 0 {
		t.Logf("dumpTaskEvents(apply=%s sid=%s): NO task.executed rows at all", applyID, sid)
		return
	}
	t.Logf("dumpTaskEvents(apply=%s sid=%s) task.executed:\n  %s", applyID, sid, strings.Join(lines, "\n  "))
}
