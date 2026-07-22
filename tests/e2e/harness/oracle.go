//go:build e2e

package harness

// L3b harness extension for Vigil/Oracle/Decree (ADR-030). Three helpers
// (CreateVigil / CreateDecree / WaitForOracleFires) + stub-emit EmitPortent —
// the minimal set through which the test drives the full path registry ->
// match-state, WITHOUT a real mTLS EventStream emit from soul-stub (that is
// a separate harness-extension slice, see tests/e2e/oracle_typed_portent_test.go).
//
// Layer-by-layer contract:
//   - CreateVigil / CreateDecree — REST POST /v1/vigils, /v1/decrees via the
//     first Archon's JWT (cluster-admin, permission `vigil.create` /
//     `decree.create` are part of the `*` set per ADR-013). Real handler
//     stack (validate.go + InsertVigil/InsertDecree), no schema bypass.
//   - WaitForOracleFires — polls the `oracle_fires` table (cooldown state per
//     (decree, subject), migration 041). UPSERT on ON CONFLICT -> one row per
//     unique (decree, subject) pair; count(rows) == "was there at least one
//     fire for each pair", NOT a cumulative fire counter for one subject
//     (repeated fires for the same subject update fired_at on the same row).
//     To count total fires, use audit_log.event_type='oracle.fired' (out of
//     scope for this helper).
//   - EmitPortent — stub: direct INSERT into `oracle_fires` (PG-only path).
//     Does NOT go through handlePortentEvent -> does NOT enqueue a scenario,
//     does NOT write audit `oracle.fired`. Purpose: drive the 3 helpers
//     end-to-end in the smoke test (TestL3b_VigilDecreeOracleFlow_Smoke). The
//     real path (soul-stub.SendPortent -> mTLS EventStream -> handlePortentEvent ->
//     SubjectMatches -> where-CEL -> EnqueueScenario -> RecordFire -> audit) is
//     covered by execution-e2e TestOracle_FileChanged_FiresScenario (vigil_oracle_test.go),
//     which also exercises WaitForOracleReaction (asserting the apply_run enqueued by the reactor).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- CreateVigil ------------------------------------------------------

// CreateVigilOpts — parameters for [Stack.CreateVigil]. The subject is XOR
// Coven/SID (CHECK vigils_subject_xor, migration 041); validated at the
// keeper's service layer, the harness checks nothing before the round trip
// (we test the public OpenAPI contract as a black box).
//
//   - Name — vigils.name (kebab-case 1..63).
//   - Interval — Soul Stack duration convention ("30s"/"5m"; config.ParseDuration).
//   - Check — core-beacon address (`core.beacon.<name>`; shared/beaconaddr.All).
//   - Coven / SID — XOR subject.
//   - Params — opaque JSON check parameters; shape depends on Check.
//   - Enabled — defaults to true (same as REST with an empty enabled field).
type CreateVigilOpts struct {
	Name     string
	Interval string
	Check    string
	Coven    []string
	SID      *string
	Params   map[string]any
	Enabled  *bool
}

// CreateVigil creates a Vigil via the Operator-API (POST /v1/vigils) and
// returns vigils.name. Any non-201 -> t.Fatal with the response body (4xx
// diagnosis without guessing, like [Stack.CreateIncarnation]).
//
// The optional IncarnationName argument is NOT passed here (a Vigil is not
// bound to an incarnation in ADR-030; incarnation_name is a Decree field).
func (s *Stack) CreateVigil(ctx context.Context, t *testing.T, opts CreateVigilOpts) string {
	t.Helper()

	body := map[string]any{
		"name":     opts.Name,
		"interval": opts.Interval,
		"check":    opts.Check,
	}
	if len(opts.Coven) > 0 {
		body["coven"] = opts.Coven
	}
	if opts.SID != nil {
		body["sid"] = *opts.SID
	}
	if opts.Params != nil {
		raw, err := json.Marshal(opts.Params)
		if err != nil {
			t.Fatalf("CreateVigil(%s): marshal params: %v", opts.Name, err)
		}
		body["params"] = json.RawMessage(raw)
	}
	if opts.Enabled != nil {
		body["enabled"] = *opts.Enabled
	}

	resp, status, err := s.opClient(t).post(ctx, "/v1/vigils", body)
	if err != nil {
		t.Fatalf("CreateVigil(%s): http: %v", opts.Name, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("CreateVigil(%s): status %d, body=%s", opts.Name, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateVigil(%s): decode: %v (body=%s)", opts.Name, err, string(resp))
	}
	if out.Name == "" {
		t.Fatalf("CreateVigil(%s): empty name in 201 body=%s", opts.Name, string(resp))
	}
	return out.Name
}

// --- CreateDecree -----------------------------------------------------

// CreateDecreeOpts — parameters for [Stack.CreateDecree].
//
//   - Name — decrees.name (kebab-case 1..63).
//   - OnBeacon — name of the Vigil (decrees.on_beacon) whose Portent this
//     rule reacts to. No FK to vigils in the schema (Decree is a managed
//     registry that survives Vigil recreation), but the name must match a
//     real Vigil grammatically, otherwise it will never match.
//   - WhereCEL — optional predicate over the event payload (typed payload
//     V5-1 or legacy event.data); empty -> always matches (subject already
//     filtered). Compiled on create via WhereCompiler (broken CEL -> 422).
//   - Coven / SID — XOR subject of the Decree (independent of the Vigil's
//     subject).
//   - IncarnationName — target incarnation of the reaction
//     (decrees.incarnation_name, required). On enqueue, membership is
//     checked: incarnation_name in the sender's covens (ADR-030(b) protects
//     against cross-incarnation escalation).
//   - ActionScenario — named scenario name (whitelist; raw command rejected,
//     ADR-030(b)). Snake_case pattern (`^[a-z][a-z0-9_]*$`).
//   - ActionInput — opaque JSON scenario input (vault-ref AS-IS, invariant A
//     of ADR-027).
//   - Cooldown — duration convention, minimum interval between fires
//     per-(decree, subject). Empty string -> DEFAULT '0s' (cooldown OFF).
//   - Enabled — defaults to true.
type CreateDecreeOpts struct {
	Name            string
	OnBeacon        string
	WhereCEL        string
	Coven           []string
	SID             *string
	IncarnationName string
	ActionScenario  string
	ActionInput     map[string]any
	Cooldown        string
	Enabled         *bool
}

// CreateDecree creates a Decree via the Operator-API (POST /v1/decrees) and
// returns decrees.name. Any non-201 -> t.Fatal.
func (s *Stack) CreateDecree(ctx context.Context, t *testing.T, opts CreateDecreeOpts) string {
	t.Helper()

	body := map[string]any{
		"name":             opts.Name,
		"on_beacon":        opts.OnBeacon,
		"incarnation_name": opts.IncarnationName,
		"action_scenario":  opts.ActionScenario,
	}
	if opts.WhereCEL != "" {
		body["where"] = opts.WhereCEL
	}
	if len(opts.Coven) > 0 {
		body["coven"] = opts.Coven
	}
	if opts.SID != nil {
		body["sid"] = *opts.SID
	}
	if opts.ActionInput != nil {
		raw, err := json.Marshal(opts.ActionInput)
		if err != nil {
			t.Fatalf("CreateDecree(%s): marshal action_input: %v", opts.Name, err)
		}
		body["action_input"] = json.RawMessage(raw)
	}
	if opts.Cooldown != "" {
		body["cooldown"] = opts.Cooldown
	}
	if opts.Enabled != nil {
		body["enabled"] = *opts.Enabled
	}

	resp, status, err := s.opClient(t).post(ctx, "/v1/decrees", body)
	if err != nil {
		t.Fatalf("CreateDecree(%s): http: %v", opts.Name, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("CreateDecree(%s): status %d, body=%s", opts.Name, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateDecree(%s): decode: %v (body=%s)", opts.Name, err, string(resp))
	}
	if out.Name == "" {
		t.Fatalf("CreateDecree(%s): empty name in 201 body=%s", opts.Name, string(resp))
	}
	return out.Name
}

// --- WaitForOracleFires -----------------------------------------------

// OracleFire — one row of the `oracle_fires` table (migration 041, cooldown
// state per-(decree, subject)). Authoritative schema is PRIMARY KEY (decree,
// subject): one row per unique pair, UPSERT on ON CONFLICT updates fired_at
// instead of adding a second row. Accordingly, len([]OracleFire) is the
// number of unique subjects a Decree has already fired for, not a
// cumulative fire counter (see the caveats in the file header).
type OracleFire struct {
	Decree  string
	Subject string
	FiredAt time.Time
}

// WaitForOracleFires blocks until the `oracle_fires` table has at least
// expectedCount rows for Decree decreeName, or until timeout. Returns the
// actual list of rows (sorted by subject ASC for deterministic asserts by
// the caller).
//
// expectedCount semantics: the number of UNIQUE subjects in
// (decree=decreeName, subject=*), NOT a cumulative fire counter (see the
// OracleFire shape).
//
// Polls every 250ms, like [Stack.WaitApplySuccess]; hard ceiling is
// timeout. Timeout expiry -> t.Fatal with a dump of the current row set (no
// hoping it "resolves itself").
func (s *Stack) WaitForOracleFires(ctx context.Context, t *testing.T, decreeName string, expectedCount int, timeout time.Duration) []OracleFire {
	t.Helper()
	if expectedCount < 1 {
		t.Fatalf("WaitForOracleFires(%s): expectedCount must be >= 1, got %d", decreeName, expectedCount)
	}

	deadline := time.Now().Add(timeout)
	var last []OracleFire
	for time.Now().Before(deadline) {
		fires, err := s.listOracleFires(ctx, decreeName)
		if err != nil {
			t.Fatalf("WaitForOracleFires(%s): query oracle_fires: %v", decreeName, err)
		}
		if len(fires) >= expectedCount {
			return fires
		}
		last = fires
		select {
		case <-ctx.Done():
			t.Fatalf("WaitForOracleFires(%s): ctx done before %d fires (got %d): %v",
				decreeName, expectedCount, len(fires), ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("WaitForOracleFires(%s): did not reach %d fires within %s (current set: %+v)",
		decreeName, expectedCount, timeout, last)
	return nil // unreachable: t.Fatalf does not return.
}

// WaitForOracleReaction blocks until at least one apply_runs row enqueued by
// the Oracle reactor appears (or until timeout): scenario=scenarioName,
// incarnation_name=incarnationName, started_by_aid IS NULL (a Soul-initiated
// reaction without an Archon identity, see oracle_enqueuer.go), and
// started_at >= since (excludes the incarnation's auto-create run). Returns
// the apply_id of the first such run (for further asserts by the caller).
// timeout -> t.Fatal.
//
// scenarioName MUST differ from the incarnation's auto-create scenario,
// otherwise the scenario+started_by_aid+since filter can't distinguish
// them. Polls every 250ms (same as WaitForOracleFires).
func (s *Stack) WaitForOracleReaction(ctx context.Context, t *testing.T, incarnationName, scenarioName string, since time.Time, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		applyID, found, err := s.findOracleReaction(ctx, incarnationName, scenarioName, since)
		if err != nil {
			t.Fatalf("WaitForOracleReaction(%s/%s): query apply_runs: %v", incarnationName, scenarioName, err)
		}
		if found {
			return applyID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WaitForOracleReaction(%s/%s): ctx done before an apply_run appeared: %v",
				incarnationName, scenarioName, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("WaitForOracleReaction(%s/%s): no apply_run from the reactor appeared within %s",
		incarnationName, scenarioName, timeout)
	return "" // unreachable: t.Fatalf does not return.
}

func (s *Stack) findOracleReaction(ctx context.Context, incarnationName, scenarioName string, since time.Time) (string, bool, error) {
	const sql = `
SELECT apply_id
FROM apply_runs
WHERE incarnation_name = $1
  AND scenario = $2
  AND started_by_aid IS NULL
  AND started_at >= $3
ORDER BY started_at ASC
LIMIT 1`
	var applyID string
	err := s.db.QueryRow(ctx, sql, incarnationName, scenarioName, since).Scan(&applyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return applyID, true, nil
}

func (s *Stack) listOracleFires(ctx context.Context, decreeName string) ([]OracleFire, error) {
	const sql = `
SELECT decree, subject, fired_at
FROM oracle_fires
WHERE decree = $1
ORDER BY subject ASC`
	rows, err := s.db.Query(ctx, sql, decreeName)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []OracleFire
	for rows.Next() {
		var f OracleFire
		if err := rows.Scan(&f.Decree, &f.Subject, &f.FiredAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}
	return out, nil
}

// --- EmitPortent (stub) -----------------------------------------------

// EmitPortent — STUB emulation of a Vigil-Decree pair firing: direct UPSERT
// into `oracle_fires` (decree, subject, fired_at=NOW). Does NOT go through
// handlePortentEvent — does NOT enqueue a scenario, does NOT write audit
// `oracle.fired`, does NOT increment the circuit breaker.
//
// Purpose: smoke-test the 3 helpers (CreateVigil / CreateDecree /
// WaitForOracleFires) end-to-end without depending on a mTLS EventStream
// emit from soul-stub (a separate harness-extension slice; we can't yet
// emulate the soul stream, so `tests/e2e/oracle_typed_portent_test.go`
// skips the full loop).
//
// Subject is the authoritative SID of the sending host (in the real path —
// from the mTLS peer cert, the harness supplies it explicitly). DecreeName
// must exist in `decrees` (FK oracle_fires.decree -> decrees(name)) —
// otherwise the INSERT fails with an FK violation.
//
// Once a full soul-stub emit (`SoulStub.SendPortent`) exists, this helper
// is removed in favor of the real path.
func (s *Stack) EmitPortent(ctx context.Context, t *testing.T, decreeName, subjectSID string) {
	t.Helper()

	const sql = `
INSERT INTO oracle_fires (decree, subject, fired_at)
VALUES ($1, $2, NOW())
ON CONFLICT (decree, subject) DO UPDATE SET fired_at = EXCLUDED.fired_at`
	if _, err := s.db.Exec(ctx, sql, decreeName, subjectSID); err != nil {
		t.Fatalf("EmitPortent(decree=%s subject=%s): insert oracle_fires: %v",
			decreeName, subjectSID, err)
	}
}
