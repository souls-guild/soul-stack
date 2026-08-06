package handlers

// Guard tests for the roster a create request carries (NIM-371): a create scenario
// declaring `source: { roster: true }` on an input field gets that field's SIDs bound
// into `incarnation_membership` before its bootstrap run starts.
//
// The invariants, in the order they matter:
//
//  1. ORDER. Membership exists BEFORE runner.Start. A run resolves its roster from
//     that relation at start, so binding afterwards would be a run into an empty
//     roster with the hosts arriving too late — the whole defect this ticket is
//     about. Pinned by a starter that inspects the DB at the moment it is called.
//  2. SCOPE. A SID outside the caller's `soul.list` purview refuses the WHOLE create
//     — no row, no run, no partial roster. Otherwise create is the way around the
//     per-host gate of NIM-209.
//  3. bind-member. A caller holding `incarnation.create` but not
//     `incarnation.bind-member` cannot populate an incarnation at birth either.
//  4. NO DECLARATION, NO BIND. A scenario that declares no roster is untouched: the
//     create path behaves exactly as it did before this feature.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- snapshot -------------------------------------------------------------

// rosterScenarioSnapshot writes a service snapshot with two create scenarios:
//
//   - create_from_souls — declares `hosts` as its roster (the shape this ticket
//     introduces: array of `format: sid` with `source: { roster: true }`);
//   - create — declares no roster (the provisioning twin), so the same handler run
//     proves the untouched path.
func rosterScenarioSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, yaml string) {
		dir := filepath.Join(root, "scenario", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write %s/main.yml: %v", name, err)
		}
	}
	write("create_from_souls", `name: create_from_souls
create: true
input:
  hosts:
    type: array
    required: true
    min_items: 1
    items:
      type: string
      format: sid
      source: { roster: true }
tasks: []
`)
	write("create", `name: create
create: true
input:
  replicas:
    type: integer
    default: 1
tasks: []
`)
	return root
}

// --- fake DB --------------------------------------------------------------

// fakeRosterDB answers the queries the create-with-roster path issues: the souls
// lookup (screening), the incarnation INSERT, and the membership INSERT ...
// RETURNING. It records WHEN the membership write happened relative to the run, which
// is what invariant 1 is about.
type fakeRosterDB struct {
	hosts []memberHost

	insertedIncarnation bool
	// insertArgs — the arguments of the incarnation INSERT. Read them by NAME via
	// the captured SQL, never by a bare position: the column list changed under
	// this fixture once already (NIM-410 dropped `spec`, and $5 silently became
	// `state` while the assertion kept calling it spec).
	insertArgs []any
	// insertSQL — the incarnation INSERT as issued. A column list is the one thing
	// a fake can be held to: it is the statement the real database would reject.
	insertSQL string
	// boundSIDs — what the membership INSERT was asked to write. nil until it runs,
	// which is how a starter can tell "roster already bound" from "not yet".
	boundSIDs []string
	// membershipInsertCalls — guards against a second bind slipping in.
	membershipInsertCalls int
	// membershipErr — when set, the membership INSERT fails (the infrastructural
	// failure branch).
	membershipErr error
}

func (f *fakeRosterDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeRosterDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO incarnation") {
		f.insertedIncarnation = true
		f.insertArgs = args
		f.insertSQL = sql
		// RETURNING created_at, updated_at (incarnation.Create scans exactly these two).
		return staticRow{values: []any{time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()}}
	}
	if strings.Contains(sql, "FROM souls") {
		for _, h := range f.hosts {
			if len(args) > 0 && h.sid == args[0].(string) {
				return memberSoulRow{h: h}
			}
		}
		return memberErrRow{pgx.ErrNoRows}
	}
	return memberErrRow{pgx.ErrNoRows}
}

func (f *fakeRosterDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation_membership"):
		f.membershipInsertCalls++
		if f.membershipErr != nil {
			return nil, f.membershipErr
		}
		sids, _ := args[1].([]string)
		f.boundSIDs = append([]string(nil), sids...)
		rows := make([][]any, 0, len(sids))
		for _, sid := range sids {
			rows = append(rows, []any{sid})
		}
		return &memberRows{rows: rows}, nil
	case strings.Contains(sql, "FROM souls"):
		want := map[string]struct{}{}
		if len(args) > 0 {
			if sids, ok := args[0].([]string); ok {
				for _, s := range sids {
					want[s] = struct{}{}
				}
			}
		}
		rows := make([][]any, 0, len(f.hosts))
		for _, h := range f.hosts {
			if _, ok := want[h.sid]; !ok {
				continue
			}
			traits, _ := json.Marshal(h.traits)
			if len(h.traits) == 0 {
				traits = nil
			}
			rows = append(rows, []any{
				h.sid, "agent", h.status, h.covens, traits,
				time.Unix(0, 0).UTC(), (*time.Time)(nil), (*string)(nil),
				(*string)(nil), (*time.Time)(nil), (*string)(nil),
			})
		}
		return &memberRows{rows: rows}, nil
	}
	return &memberRows{}, nil
}

func (f *fakeRosterDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("fakeRosterDB: BeginTx not used by the create path")
}

// --- helpers --------------------------------------------------------------

// rosterHandler wires the full create path with a roster-declaring snapshot.
func rosterHandler(t *testing.T, db *fakeRosterDB, starter ScenarioStarter, scoper PurviewResolver, auditW audit.Writer) *IncarnationHandler {
	t.Helper()
	loader := &fakeLoader{localDir: rosterScenarioSnapshot(t)}
	return NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, loader, auditW, scoper, nil)
}

// createFromSoulsBody — a create request against the roster-declaring scenario.
func createFromSoulsBody(sids ...string) *bytes.Reader {
	body, _ := json.Marshal(map[string]any{
		"name":            "redis-roster",
		"service":         "redis",
		"covens":          []string{"prod"},
		"create_scenario": "create_from_souls",
		"input":           map[string]any{"hosts": sids},
	})
	return bytes.NewReader(body)
}

// rosterAwareStarter records the membership state AS SEEN at Start time — the only
// way to assert the bind/run ORDER rather than merely that both happened.
type rosterAwareStarter struct {
	db *fakeRosterDB

	calls          int
	boundAtStart   []string
	gotSpec        scenario.RunSpec
	incAtStartSeen bool
}

func (s *rosterAwareStarter) Start(_ context.Context, spec scenario.RunSpec) error {
	s.calls++
	s.gotSpec = spec
	s.boundAtStart = append([]string(nil), s.db.boundSIDs...)
	s.incAtStartSeen = s.db.insertedIncarnation
	return nil
}

// bindMemberDenyingChecker admits `incarnation.create` and refuses
// `incarnation.bind-member` — the role shape invariant 3 is about.
type bindMemberDenyingChecker struct{}

var errBindMemberDenied = errors.New("bind-member denied")

func (bindMemberDenyingChecker) Check(_, resource, action string, _ map[string]string) error {
	if resource == "incarnation" && action == "bind-member" {
		return errBindMemberDenied
	}
	return nil
}

// --- invariant 1: the roster is bound BEFORE the run starts ---------------

func TestCreateRoster_BoundBeforeRunStarts(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
		{sid: "node-2.example.com", status: "connected", covens: []string{"prod"}},
	}}
	starter := &rosterAwareStarter{db: db}
	h := rosterHandler(t, db, starter, memberScoper{unrestricted: true}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		createFromSoulsBody("node-2.example.com", "node-1.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1", starter.calls)
	}
	// THE assertion of this ticket: at the moment the run started, the roster was
	// already in `incarnation_membership`. A regression here is a run into no_hosts.
	if len(starter.boundAtStart) != 2 {
		t.Fatalf("roster at Start = %v, want both SIDs bound BEFORE the run", starter.boundAtStart)
	}
	if !starter.incAtStartSeen {
		t.Error("incarnation row must exist before the run starts")
	}
	// Sorted and de-duplicated, whatever order the operator picked them in.
	if db.boundSIDs[0] != "node-1.example.com" || db.boundSIDs[1] != "node-2.example.com" {
		t.Errorf("bound SIDs = %v, want sorted", db.boundSIDs)
	}
	if db.membershipInsertCalls != 1 {
		t.Errorf("membership INSERT calls = %d, want exactly 1", db.membershipInsertCalls)
	}
}

// The roster reaches the run as MEMBERSHIP, not as a spec.hosts declaration: spec
// carries the operator's input verbatim and nothing else. Guards against "fixing"
// this by writing spec.hosts, which reads plausible and binds nothing (the resolver
// joins incarnation_membership; spec.hosts only names roles).
func TestCreateRoster_DoesNotWriteSpecHosts(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
	}}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", createFromSoulsBody("node-1.example.com"))
	req = withClaims(req, "archon-alice")
	if rec := incCreate(h, req); rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if len(db.boundSIDs) != 1 {
		t.Fatalf("bound SIDs = %v, want the roster bound as membership", db.boundSIDs)
	}
	// The create INSERT names no spec-like column at all. This is asserted on the
	// STATEMENT rather than on an argument, because there is no longer an argument
	// to read: NIM-410 dropped the column, and the assertion that used to live here
	// went on reading position $5 — which had become `state`. It stayed green while
	// naming a column that does not exist, which is the failure this test is about.
	//
	// The roster's home is `incarnation_membership` (bound above), and the input
	// the create ran on lives in that run's history snapshot, which is what
	// rerun-last replays from.
	if !strings.Contains(db.insertSQL, "INSERT INTO incarnation") {
		t.Fatalf("no incarnation INSERT was issued; SQL = %q", db.insertSQL)
	}
	for _, banned := range []string{"spec", "hosts"} {
		if strings.Contains(db.insertSQL, banned) {
			t.Errorf("create INSERT names %q — the roster is membership and the "+
				"declared-hosts blob is gone (NIM-330, NIM-410):\n%s", banned, db.insertSQL)
		}
	}
}

// --- invariant 2: a host outside the caller's soul scope refuses the create ---

func TestCreateRoster_ForeignHostRefusesWholeCreate(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
		{sid: "foreign.example.com", status: "connected", covens: []string{"other-team"}},
	}}
	starter := &fakeStarter{}
	h := rosterHandler(t, db, starter, memberScoper{coven: "prod"}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		createFromSoulsBody("node-1.example.com", "foreign.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Code = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "foreign.example.com") {
		t.Errorf("the refusal must name the offending SID, got: %s", rec.Body.String())
	}
	// All-or-nothing, and nothing at all: no incarnation, no partial roster, no run.
	if db.insertedIncarnation {
		t.Error("an out-of-scope roster must not create the incarnation")
	}
	if db.membershipInsertCalls != 0 {
		t.Errorf("membership INSERT calls = %d, want 0 (no partial bind)", db.membershipInsertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0", starter.calls)
	}
}

// An unknown SID is 422, and it is answered BEFORE the scope bucket only for hosts
// that genuinely do not exist — a host the operator may not see is still "forbidden"
// (checked above), never "unknown". This pins the bucket order shared with the bind
// route, where it is a security property rather than cosmetics.
func TestCreateRoster_UnknownSIDIs422(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
	}}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		createFromSoulsBody("node-1.example.com", "ghost.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertedIncarnation {
		t.Error("an unknown SID in the roster must not create the incarnation")
	}
}

// Only an onboarded, CONNECTED host can carry a deployment — a disconnected one in
// the roster is 422, refused before the row exists.
func TestCreateRoster_DisconnectedHostIs422(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
		{sid: "node-2.example.com", status: "disconnected", covens: []string{"prod"}},
	}}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		createFromSoulsBody("node-1.example.com", "node-2.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "node-2.example.com") {
		t.Errorf("the refusal must name the offending SID, got: %s", rec.Body.String())
	}
	if db.insertedIncarnation {
		t.Error("a disconnected host in the roster must not create the incarnation")
	}
}

// --- invariant 3: create alone does not let you populate --------------------

func TestCreateRoster_WithoutBindMemberPermissionIs403(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
	}}
	starter := &fakeStarter{}
	h := rosterHandler(t, db, starter, memberScoper{unrestricted: true}, &memberAuditCapture{})
	h.SetPermissionChecker(bindMemberDenyingChecker{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", createFromSoulsBody("node-1.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Code = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bind-member") {
		t.Errorf("the refusal must name the missing permission, got: %s", rec.Body.String())
	}
	if db.insertedIncarnation || starter.calls != 0 {
		t.Error("a create refused on bind-member must not insert or start anything")
	}
}

// A create WITHOUT a roster is unaffected by the bind-member gate: the permission is
// asked about populating an incarnation, not about creating one.
func TestCreateWithoutRoster_UnaffectedByBindMemberPermission(t *testing.T) {
	db := &fakeRosterDB{}
	starter := &fakeStarter{}
	h := rosterHandler(t, db, starter, memberScoper{unrestricted: true}, &memberAuditCapture{})
	h.SetPermissionChecker(bindMemberDenyingChecker{})

	body, _ := json.Marshal(map[string]any{
		"name": "redis-plain", "service": "redis", "create_scenario": "create",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", bytes.NewReader(body))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1", starter.calls)
	}
}

// --- invariant 4: no declaration, no bind ---------------------------------

func TestCreateRoster_ScenarioWithoutDeclarationBindsNothing(t *testing.T) {
	db := &fakeRosterDB{}
	starter := &fakeStarter{}
	h := rosterHandler(t, db, starter, memberScoper{unrestricted: true}, &memberAuditCapture{})

	body, _ := json.Marshal(map[string]any{
		"name": "redis-plain", "service": "redis", "create_scenario": "create",
		"input": map[string]any{"replicas": 2},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", bytes.NewReader(body))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if db.membershipInsertCalls != 0 {
		t.Errorf("membership INSERT calls = %d, want 0 — a scenario declaring no roster binds nothing", db.membershipInsertCalls)
	}
	if starter.calls != 1 {
		t.Errorf("starter.calls = %d, want 1", starter.calls)
	}
}

// A roster-declaring scenario with the field left empty is refused by the ORDINARY
// input gate (`required: true` + `min_items: 1`), before anything is created — the
// requiredness is the scenario's declaration, not a second rule in the keeper.
func TestCreateRoster_EmptyRosterRefusedByInputGate(t *testing.T) {
	db := &fakeRosterDB{}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})

	body, _ := json.Marshal(map[string]any{
		"name": "redis-roster", "service": "redis", "create_scenario": "create_from_souls",
		"input": map[string]any{"hosts": []string{}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", bytes.NewReader(body))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertedIncarnation {
		t.Error("an empty roster must not create the incarnation")
	}
}

// --- audit ----------------------------------------------------------------

// The roster bound at create is audited as an ordinary member bind, marked
// `via: create`. A separate event type would hide half the membership history from
// anyone asking "how did this host get into this incarnation".
func TestCreateRoster_AuditsMemberBoundViaCreate(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
	}}
	auditCap := &memberAuditCapture{}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, auditCap)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", createFromSoulsBody("node-1.example.com"))
	req = withClaims(req, "archon-alice")
	if rec := incCreate(h, req); rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	var bound *audit.Event
	for _, e := range auditCap.events {
		if e.EventType == audit.EventIncarnationMemberBound {
			bound = e
		}
	}
	if bound == nil {
		t.Fatalf("no incarnation.member_bound event, got %v", auditCap.events)
	}
	if bound.Payload["via"] != "create" {
		t.Errorf("payload.via = %v, want create", bound.Payload["via"])
	}
	if bound.ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q, want archon-alice", bound.ArchonAID)
	}
}

// --- the bind failing after the insert ------------------------------------

// The one branch where the incarnation survives a failure: the row is in, the bind
// broke. No run starts — an empty roster would abort it as no_hosts anyway, and an
// error_locked would leave the operator with an unlock to do before the actual fix.
// The message has to say what state they are in, since the 500 alone reads as
// "nothing happened".
func TestCreateRoster_BindFailureLeavesNoRun(t *testing.T) {
	db := &fakeRosterDB{
		hosts:         []memberHost{{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}}},
		membershipErr: errors.New("membership insert exploded"),
	}
	starter := &fakeStarter{}
	h := rosterHandler(t, db, starter, memberScoper{unrestricted: true}, &memberAuditCapture{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", createFromSoulsBody("node-1.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Code = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 — no run onto a roster we failed to bind", starter.calls)
	}
	if !db.insertedIncarnation {
		t.Error("the incarnation row is expected to survive — that is what makes this recoverable")
	}
	if !strings.Contains(rec.Body.String(), "members") {
		t.Errorf("the 500 must tell the operator how to recover, got: %s", rec.Body.String())
	}
}

// --- cap ------------------------------------------------------------------

// The per-request cap of the bind route applies to a create-carried roster too: it is
// the same write, and a roster is a human-sized list of hosts.
func TestCreateRoster_OverCapIs422(t *testing.T) {
	db := &fakeRosterDB{}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})

	sids := make([]string, MaxBindMembersPerRequest+1)
	for i := range sids {
		sids[i] = "node-" + strconv.Itoa(i) + ".example.com"
	}
	body, _ := json.Marshal(map[string]any{
		"name": "redis-roster", "service": "redis", "create_scenario": "create_from_souls",
		"input": map[string]any{"hosts": sids},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", bytes.NewReader(body))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertedIncarnation {
		t.Error("an over-cap roster must not create the incarnation")
	}
}

// --- ScreenCreateRoster shape --------------------------------------------

// The screening is package-level so the MCP create tool runs THIS code rather than a
// second implementation of it. Called with no declaration it must be a clean no-op —
// that is what keeps every non-roster create on both surfaces unchanged.
func TestScreenCreateRoster_NoDeclarationIsNoOp(t *testing.T) {
	res, err := ScreenCreateRoster(context.Background(), &fakeRosterDB{}, allowAllChecker{},
		memberScoper{unrestricted: true}, "archon-alice", "redis-x", "redis", nil, scenario.CreatePlan{})
	if err != nil {
		t.Fatalf("ScreenCreateRoster: %v", err)
	}
	if len(res.SIDs) != 0 || res.Rejection != nil {
		t.Errorf("screening = %+v, want empty", res)
	}
}

// Fail-closed on a missing purview resolver: without it the per-host boundary cannot
// be evaluated, and binding regardless is the escalation gate (b) exists to prevent.
func TestScreenCreateRoster_NilScoperFailsClosed(t *testing.T) {
	plan := scenario.CreatePlan{RosterField: "hosts", RosterSIDs: []string{"node-1.example.com"}}
	_, err := ScreenCreateRoster(context.Background(), &fakeRosterDB{}, allowAllChecker{},
		nil, "archon-alice", "redis-x", "redis", nil, plan)
	if err == nil {
		t.Fatal("a nil purview resolver must refuse, not bind")
	}
}

// A malformed SID is refused with the shape sentinel, so both surfaces answer 422
// rather than letting it reach the membership relation.
func TestScreenCreateRoster_InvalidSIDSentinel(t *testing.T) {
	plan := scenario.CreatePlan{RosterField: "hosts", RosterSIDs: []string{"NOT A SID"}}
	_, err := ScreenCreateRoster(context.Background(), &fakeRosterDB{}, allowAllChecker{},
		memberScoper{unrestricted: true}, "archon-alice", "redis-x", "redis", nil, plan)
	if !errors.Is(err, ErrCreateRosterInvalidSID) {
		t.Fatalf("err = %v, want ErrCreateRosterInvalidSID", err)
	}
}

// problem-type sanity: the REST projection of a scope refusal is `forbidden`, not a
// validation failure — the operator's role is what needs changing.
func TestCreateRoster_ScopeRefusalProblemType(t *testing.T) {
	db := &fakeRosterDB{hosts: []memberHost{
		{sid: "node-1.example.com", status: "connected", covens: []string{"prod"}},
	}}
	h := rosterHandler(t, db, &fakeStarter{}, memberScoper{unrestricted: true}, &memberAuditCapture{})
	h.SetPermissionChecker(bindMemberDenyingChecker{})

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", createFromSoulsBody("node-1.example.com"))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := body["type"].(string); !strings.HasSuffix(got, string(problem.TypeForbidden)) {
		t.Errorf("problem type = %q, want the forbidden URN", got)
	}
}
