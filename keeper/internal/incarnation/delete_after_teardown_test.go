package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// deleteTx builds a fakeTx for DeleteAfterTeardown. The single-winner guard
// lives in the DELETE's own WHERE status='destroying'; the one SELECT in this
// transaction is the force-path capture of the resources about to be abandoned
// (NIM-395), scripted here with an empty state so non-force tests are unaffected.
// execTags set RowsAffected for the DELETE (third Exec, idx=2).
func deleteTx(deleteTag pgconn.CommandTag) *fakeTx {
	return &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{}`)}}, // state jsonb
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"), // archive incarnation
			pgconn.NewCommandTag("INSERT 0 3"), // archive state_history
			deleteTag,                          // DELETE incarnation
		},
	}
}

// TestDeleteAfterTeardown_HappyWinner — the single-winner DELETE removed the
// row (RowsAffected==1): the archive is filled BEFORE DELETE (Exec order),
// the transaction commits, Deleted=true, an audit destroy_completed is written.
func TestDeleteAfterTeardown_HappyWinner(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true (RowsAffected==1)")
	}
	if !tx.committed {
		t.Error("tx not committed on winning DELETE")
	}
	// Note: fakeTx.Rollback always sets rolled=true (defer Rollback after Commit
	// is a standard pattern, real pgx ignores such a rollback). So we check
	// committed, not !rolled (as in the Unlock/Upgrade tests).

	// Order: archive incarnation → archive state_history → DELETE.
	if tx.execN != 3 {
		t.Fatalf("Exec calls = %d, want 3 (archive×2 + DELETE)", tx.execN)
	}
	if !strings.Contains(tx.execSQLs[0], "INSERT INTO incarnation_archive") {
		t.Errorf("Exec[0] not incarnation_archive insert: %q", tx.execSQLs[0])
	}
	if !strings.Contains(tx.execSQLs[1], "INSERT INTO state_history_archive") {
		t.Errorf("Exec[1] not state_history_archive insert: %q", tx.execSQLs[1])
	}
	if !strings.Contains(tx.execSQLs[2], "DELETE FROM incarnation") {
		t.Errorf("Exec[2] not DELETE: %q", tx.execSQLs[2])
	}
	// The archive is written BEFORE DELETE (indexes 0,1 < 2).
	if !strings.Contains(tx.execSQLs[2], "status = 'destroying'") {
		t.Errorf("DELETE missing single-winner guard status='destroying': %q", tx.execSQLs[2])
	}
	// The archive incarnation SELECT is also under the destroying guard.
	if !strings.Contains(tx.execSQLs[0], "status = 'destroying'") {
		t.Errorf("archive incarnation missing destroying guard: %q", tx.execSQLs[0])
	}
}

// TestDeleteAfterTeardown_NoOpLoser — RowsAffected==0 (someone already removed
// the row / the status changed): the single-winner lost → Deleted=false, the
// transaction does NOT commit (rollback discards the written archive), no
// audit is written. No error — an idempotent no-op.
func TestDeleteAfterTeardown_NoOpLoser(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 0"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown no-op must not error: %v", err)
	}
	if res.Deleted {
		t.Error("Deleted = true, want false (RowsAffected==0)")
	}
	if tx.committed {
		t.Error("no-op tx must NOT commit (rollback discards archive)")
	}
	if !tx.rolled {
		t.Error("no-op tx must rollback")
	}
	if len(aw.events) != 0 {
		t.Errorf("no-op wrote %d audit events, want 0", len(aw.events))
	}
}

// TestDeleteAfterTeardown_AuditCompleted — on a successful DELETE,
// incarnation.destroy_completed is written: source=keeper_internal, payload
// {name, force}, no secrets.
func TestDeleteAfterTeardown_AuditCompleted(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	if _, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil); err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationDestroyCompleted {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventIncarnationDestroyCompleted)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("Source = %q, want keeper_internal", ev.Source)
	}
	if ev.ArchonAID != "" {
		t.Errorf("ArchonAID = %q, want empty (keeper_internal, NULL column)", ev.ArchonAID)
	}
	if ev.Payload["name"] != "redis-prod" {
		t.Errorf("payload.name = %v, want redis-prod", ev.Payload["name"])
	}
	if ev.Payload["force"] != true {
		t.Errorf("payload.force = %v, want true", ev.Payload["force"])
	}
}

// TestDeleteAfterTeardown_AuditFailureDoesNotFail — an audit-write failure does
// NOT fail destroy (the row is already gone, the tx is committed).
func TestDeleteAfterTeardown_AuditFailureDoesNotFail(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{writeErr: errors.New("audit down")}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("must not fail on audit error: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true")
	}
	if !tx.committed {
		t.Error("tx must commit despite audit write failure")
	}
}

// TestDeleteAfterTeardown_NilAuditWriter — w == nil doesn't panic.
func TestDeleteAfterTeardown_NilAuditWriter(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, nil, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown with nil writer: %v", err)
	}
	if !res.Deleted {
		t.Error("Deleted = false, want true")
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
}

// TestDeleteAfterTeardown_RejectsBadName — an invalid name is rejected before
// the round trip (no transaction at all).
func TestDeleteAfterTeardown_RejectsBadName(t *testing.T) {
	pool := &fakePool{txs: []*fakeTx{deleteTx(pgconn.NewCommandTag("DELETE 1"))}}
	_, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "BAD_NAME", false, nil)
	if err == nil {
		t.Fatal("invalid name returned nil err")
	}
	if pool.beginN != 0 {
		t.Errorf("BeginTx called %d times, want 0 (validation before round-trip)", pool.beginN)
	}
}

// TestDeleteAfterTeardown_ArchiveErrorAborts — an archive-INSERT failure rolls
// back the transaction and never reaches DELETE (archive+DELETE are atomic).
func TestDeleteAfterTeardown_ArchiveErrorAborts(t *testing.T) {
	tx := &fakeTx{
		execErrAt: 0, // fail on the first Exec (archive incarnation)
		execErr:   errors.New("archive boom"),
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err == nil {
		t.Fatal("archive failure returned nil err")
	}
	if tx.committed {
		t.Error("tx must not commit on archive failure")
	}
	if !tx.rolled {
		t.Error("tx must rollback on archive failure")
	}
	// Never reached DELETE (failed on the first Exec).
	if tx.execN != 1 {
		t.Errorf("Exec calls = %d, want 1 (aborted at archive incarnation)", tx.execN)
	}
}

// --- NIM-395 guards: force must not silently mean "released" ---------------
//
// "Removing the record" and "releasing the resource" are different operations.
// force does only the first — the `destroy` scenario never runs, so the cloud
// VMs keep running and the keeper-side cascade (souls / seeds / bootstrap
// tokens) never fires. These guards fail on the pre-NIM-395 code, which copied
// the live `destroying` status into the archive and recorded nothing about what
// was left behind.

// provisionedStateJSON — an incarnation state carrying cloud coordinates, in the
// shape the `destroy` scenario feeds to core.cloud.destroyed (see the redis /
// dragonfly example services).
const provisionedStateJSON = `{
  "provisioned_provider": "example-dev",
  "provisioned_vm_ids": ["i-aaa111", "i-bbb222"],
  "provisioned_sids": ["vm-1.example.com", "vm-2.example.com"]
}`

// forceDeleteTx — deleteTx with a provisioned state and a two-host membership,
// i.e. an incarnation that force-destroy is about to abandon.
func forceDeleteTx() *fakeTx {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	tx.selectRow = scriptedRow{values: []any{[]byte(provisionedStateJSON)}}
	tx.rowsResult = &fakeRows{rows: []staticRow{
		{values: []any{"vm-1.example.com"}},
		{values: []any{"vm-2.example.com"}},
	}}
	return tx
}

// archiveInsertArgs returns the args of the incarnation_archive INSERT (Exec 0):
// [name, archive status, status_details patch].
func archiveInsertArgs(t *testing.T, tx *fakeTx) (string, []byte) {
	t.Helper()
	if len(tx.execArgs) == 0 {
		t.Fatal("no Exec recorded — archive INSERT never ran")
	}
	args := tx.execArgs[0]
	if len(args) != 3 {
		t.Fatalf("archive INSERT args = %d (%v), want 3 [name, status, details] — "+
			"a 1-arg INSERT means the archive still copies the live status", len(args), args)
	}
	status, ok := args[1].(string)
	if !ok {
		t.Fatalf("archive INSERT arg[1] = %T, want string status", args[1])
	}
	patch, ok := args[2].([]byte)
	if !ok {
		t.Fatalf("archive INSERT arg[2] = %T, want []byte status_details patch", args[2])
	}
	return status, patch
}

// TestDeleteAfterTeardown_ForceStampsTerminalArchiveStatus — GUARD: a force
// destroy archives the row as `force_destroyed`, never as the live `destroying`.
// Copying the live status stamps every archived incarnation with a non-terminal
// value that distinguishes an actual teardown from a walk-away.
func TestDeleteAfterTeardown_ForceStampsTerminalArchiveStatus(t *testing.T) {
	tx := forceDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", true, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res.ArchiveStatus != ArchiveStatusForceDestroyed {
		t.Errorf("ArchiveStatus = %q, want %q", res.ArchiveStatus, ArchiveStatusForceDestroyed)
	}

	status, _ := archiveInsertArgs(t, tx)
	if status != ArchiveStatusForceDestroyed {
		t.Errorf("archived status = %q, want %q", status, ArchiveStatusForceDestroyed)
	}
	if status == string(StatusDestroying) {
		t.Error("archived status is the live non-terminal `destroying` — the archive says nothing about the outcome")
	}
	// The SELECT feeding the archive must stamp $2, not copy the live column.
	// (The INSERT's own column list still names `status` — check the SELECT.)
	_, sel, _ := strings.Cut(tx.execSQLs[0], "SELECT name, service")
	if !strings.Contains(sel, "state, $2,") {
		t.Errorf("archive SELECT still copies `status` from the live row: %q", sel)
	}
}

// TestDeleteAfterTeardown_TeardownStampsDestroyedArchiveStatus — the mirror
// case: teardown ran (force=false), so the archive records `destroyed`. Both
// paths must be distinguishable in the archive, which is the whole point.
func TestDeleteAfterTeardown_TeardownStampsDestroyedArchiveStatus(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", false, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res.ArchiveStatus != ArchiveStatusDestroyed {
		t.Errorf("ArchiveStatus = %q, want %q", res.ArchiveStatus, ArchiveStatusDestroyed)
	}
	if res.Unreleased != nil {
		t.Errorf("Unreleased = %+v, want nil after a real teardown", res.Unreleased)
	}
	status, patch := archiveInsertArgs(t, tx)
	if status != ArchiveStatusDestroyed {
		t.Errorf("archived status = %q, want %q", status, ArchiveStatusDestroyed)
	}
	if strings.Contains(string(patch), "unreleased") {
		t.Errorf("status_details patch = %s, want no `unreleased` key on the teardown path", patch)
	}
}

// TestDeleteAfterTeardown_ForceRecordsUnreleasedResources — GUARD, the core of
// NIM-395: a force destroy records the cloud VMs and member hosts it did NOT
// release, both to the caller and into the archived status_details. The member
// SIDs come from `incarnation_membership`, which the FK cascade on DELETE wipes
// — capture it in this transaction or it is gone for good.
func TestDeleteAfterTeardown_ForceRecordsUnreleasedResources(t *testing.T) {
	tx := forceDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", true, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res.Unreleased == nil {
		t.Fatal("Unreleased = nil — force destroyed the record and reported nothing left behind")
	}
	if res.Unreleased.IsEmpty() {
		t.Fatal("Unreleased is empty — the abandoned VMs and hosts were not recorded")
	}
	if res.Unreleased.Provider != "example-dev" {
		t.Errorf("Unreleased.Provider = %q, want example-dev", res.Unreleased.Provider)
	}
	if got, want := strings.Join(res.Unreleased.VMIDs, ","), "i-aaa111,i-bbb222"; got != want {
		t.Errorf("Unreleased.VMIDs = %q, want %q — these machines are still running at the provider", got, want)
	}
	if got, want := strings.Join(res.Unreleased.SIDs, ","), "vm-1.example.com,vm-2.example.com"; got != want {
		t.Errorf("Unreleased.SIDs = %q, want %q (from incarnation_membership, deleted by the cascade)", got, want)
	}

	// The same set must be durable: the archive is what survives the request.
	_, patch := archiveInsertArgs(t, tx)
	var details map[string]any
	if err := json.Unmarshal(patch, &details); err != nil {
		t.Fatalf("status_details patch is not JSON (%s): %v", patch, err)
	}
	if _, ok := details["unreleased"]; !ok {
		t.Fatalf("status_details patch = %s, want an `unreleased` key", patch)
	}
	for _, want := range []string{"i-aaa111", "i-bbb222", "vm-1.example.com", "example-dev"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("archived status_details %s missing %q", patch, want)
		}
	}

	// Every scripted membership row was consumed, i.e. the roster was not read
	// short. Ordering is pinned separately, by
	// TestDeleteAfterTeardown_ForceCapturesBeforeAnyMutation.
	if tx.rowsResult.idx != 2 {
		t.Errorf("membership rows consumed = %d, want 2 — the roster was read short", tx.rowsResult.idx)
	}
}

// TestDeleteAfterTeardown_ForceCapturesBeforeAnyMutation — GUARD: the capture
// must happen BEFORE the first mutating statement. `incarnation_membership` is
// wiped by the DELETE's FK cascade and the incarnation row goes with it, so a
// capture moved below either one reads an empty set — and an empty set is
// reported as "force ran, nothing was left behind", which is the failure this
// ticket exists to remove, restated one statement later.
//
// Honest about its own worth: today this test is defense in depth, not the only
// net. Measured with `go test -overlay`, a statement inserted ahead of the
// capture also reddens ten other tests in this file, because they assert the
// Exec count and the SQL at each index. The archive INSERT consumes the capture
// as an argument, so the compiler pins the ordering as well.
//
// It is here because both of those pin the invariant INCIDENTALLY. They are
// assertions about how many statements run and in what order, and whoever adds
// a statement will update them as a matter of course — at which point the
// ordering stops being checked and nothing says so. This test fails for the
// right reason and names it in the message.
//
// Checked against the recorded statement sequence rather than against the data,
// deliberately: the fake replays its scripted rows whenever it is asked, so no
// value-level assertion in this file can see a reordering at all.
// [TestIntegration_DeleteAfterTeardown_ForceRecordsAbandonedResources] sees it
// against a real cascade — but that one needs a database, and the local gate
// runs without one.
func TestDeleteAfterTeardown_ForceCapturesBeforeAnyMutation(t *testing.T) {
	tx := forceDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}

	if _, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", true, nil); err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}

	firstExec := slices.Index(tx.calls, "exec")
	if firstExec < 0 {
		t.Fatalf("calls = %v — no mutating statement ran at all", tx.calls)
	}
	// The two halves of the capture: state (provider + vm_ids) via QueryRow,
	// roster (sids) via Query. Both are dimensions of their own — a service that
	// provisions no cloud still leaves hosts — so both must clear the mutation.
	for _, kind := range []string{"queryrow", "query"} {
		at := slices.Index(tx.calls, kind)
		if at < 0 {
			t.Errorf("calls = %v — %s never ran, so that half of the capture is missing", tx.calls, kind)
			continue
		}
		if at > firstExec {
			t.Errorf("calls = %v — %s at %d runs AFTER the first mutation at %d; "+
				"by then the cascade has taken its source and the capture reads empty",
				tx.calls, kind, at, firstExec)
		}
	}
}

// TestDeleteAfterTeardown_ForceAuditCarriesUnreleased — GUARD: the audit trail
// carries the abandoned resources. `incarnation_archive` has no read API, so
// `incarnation.destroy_completed` is the operator-reachable record of what a
// force destroy left running.
func TestDeleteAfterTeardown_ForceAuditCarriesUnreleased(t *testing.T) {
	tx := forceDeleteTx()
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	if _, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil); err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	p := aw.events[0].Payload
	if p["archive_status"] != ArchiveStatusForceDestroyed {
		t.Errorf("payload.archive_status = %v, want %q", p["archive_status"], ArchiveStatusForceDestroyed)
	}
	if p["teardown"] != "skipped" {
		t.Errorf("payload.teardown = %v, want \"skipped\"", p["teardown"])
	}
	// A map, not the struct: see
	// TestDeleteAfterTeardown_ForceAuditPayloadOmitsEmptyDimensions for why the
	// shape itself is load-bearing.
	u, ok := p["unreleased"].(map[string]any)
	if !ok {
		t.Fatalf("payload.unreleased = %T (%v), want map[string]any — "+
			"the audit trail must name what was left running", p["unreleased"], p["unreleased"])
	}
	vmIDs, _ := u["vm_ids"].([]any)
	sids, _ := u["sids"].([]any)
	if len(vmIDs) != 2 || len(sids) != 2 {
		t.Errorf("payload.unreleased = %+v, want 2 vm_ids and 2 sids", u)
	}
}

// TestDeleteAfterTeardown_ForceWithNothingProvisioned — force on an incarnation
// that provisioned nothing still records the skip via `force_destroyed`, but
// reports NO unreleased resources at all. "Nothing to abandon" and "teardown ran"
// stay different outcomes (the archive status separates them); an empty record
// would leave the operator deciding whether `{}` means "checked, clean" or
// "could not tell", so it is absent instead — which is what the docs promise.
func TestDeleteAfterTeardown_ForceWithNothingProvisioned(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1")) // empty state, no members
	pool := &fakePool{txs: []*fakeTx{tx}}
	w := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, w, "redis-prod", true, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res.ArchiveStatus != ArchiveStatusForceDestroyed {
		t.Errorf("ArchiveStatus = %q, want %q", res.ArchiveStatus, ArchiveStatusForceDestroyed)
	}
	if res.Unreleased != nil {
		t.Errorf("Unreleased = %+v, want nil — nothing was provisioned", res.Unreleased)
	}

	_, patch := archiveInsertArgs(t, tx)
	if string(patch) != `{}` {
		t.Errorf("status_details patch = %s, want {} — an empty `unreleased` must not be stored", patch)
	}
	if len(w.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(w.events))
	}
	p := w.events[0].Payload
	if _, ok := p["unreleased"]; ok {
		t.Errorf("audit payload carries `unreleased` = %v, want the key absent", p["unreleased"])
	}
	if p["teardown"] != "skipped" {
		t.Errorf("audit payload teardown = %v, want \"skipped\" — the skip is recorded either way", p["teardown"])
	}
}

// TestDeleteAfterTeardown_ForceRecordsEachDimensionAlone — GUARD: any ONE
// non-empty dimension is enough to keep the record. Since NIM-395's follow-up
// made emptiness the sole gate on whether `unreleased` exists at all, a variant
// of IsEmpty that stops counting one field silently restores the original defect
// for every incarnation whose only abandoned resource is of that kind — and the
// fixture used by the other guards carries all three at once, so each field
// there is covered by the other two.
//
// The first case is not hypothetical: a service that provisions no cloud
// machines (`create_from_souls`) has member hosts and nothing else, and the
// convention keys are explicitly empty on that path. Force there walks away
// from registered souls, un-orphaned seeds and unburnt bootstrap tokens, and
// the roster naming them is what the cascade wipes.
func TestDeleteAfterTeardown_ForceRecordsEachDimensionAlone(t *testing.T) {
	cases := []struct {
		name   string
		field  string // the json name of the dimension this case isolates
		state  string
		roster *fakeRows
		wantIn string // must reach the archived status_details
	}{
		{
			name:   "member hosts only",
			field:  "sids",
			state:  `{}`,
			roster: &fakeRows{rows: []staticRow{{values: []any{"vm-1.example.com"}}}},
			wantIn: "vm-1.example.com",
		},
		{
			name:   "provider only",
			field:  "provider",
			state:  `{"provisioned_provider": "example-dev"}`,
			wantIn: "example-dev",
		},
		{
			name:   "vm ids only",
			field:  "vm_ids",
			state:  `{"provisioned_vm_ids": ["i-aaa111"]}`,
			wantIn: "i-aaa111",
		},
	}
	// The table is hand-written; the domain is not. A fourth dimension added to
	// [UnreleasedResources] is carried to the archive and the audit by the
	// marshal and to both wire types by the reflect guards in internal/api and
	// internal/mcp — but nothing would check that it ALONE keeps the record
	// alive, which is the axis this test exists for. Then IsEmpty counting a
	// field that collectUnreleased never fills (or the reverse) passes in
	// silence, and a force that abandoned only that kind of resource reports
	// nothing at all.
	covered := make([]string, 0, len(cases))
	for _, tc := range cases {
		covered = append(covered, tc.field)
	}
	slices.Sort(covered)
	if want := domainDimensions(t); !slices.Equal(covered, want) {
		t.Errorf("cases isolate %v, the domain records %v — an untested dimension may not hold the record on its own",
			covered, want)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
			tx.selectRow = scriptedRow{values: []any{[]byte(tc.state)}}
			tx.rowsResult = tc.roster
			pool := &fakePool{txs: []*fakeTx{tx}}
			aw := &fakeAuditWriter{}

			res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil)
			if err != nil {
				t.Fatalf("DeleteAfterTeardown: %v", err)
			}
			if res.Unreleased == nil {
				t.Fatal("Unreleased = nil — one dimension is still something left behind")
			}
			if res.Unreleased.IsEmpty() {
				t.Fatalf("IsEmpty() = true for %+v — the operator is told nothing survived", res.Unreleased)
			}
			_, patch := archiveInsertArgs(t, tx)
			if !strings.Contains(string(patch), tc.wantIn) {
				t.Errorf("archived status_details = %s, want it to name %q", patch, tc.wantIn)
			}
			if len(aw.events) != 1 {
				t.Fatalf("audit events = %d, want 1", len(aw.events))
			}
			if _, ok := aw.events[0].Payload["unreleased"]; !ok {
				t.Error("audit payload has no `unreleased` key — this force abandoned something")
			}
		})
	}
}

// domainDimensions — sorted json names of the fields [UnreleasedResources] records.
func domainDimensions(t *testing.T) []string {
	t.Helper()
	rt := reflect.TypeOf(UnreleasedResources{})
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("UnreleasedResources.%s carries no json name", rt.Field(i).Name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestDeleteAfterTeardown_ForceMasksStateDerivedValues — GUARD: the two fields
// read out of `incarnation.state` are masked before they leave this package.
//
// `state` is service-authored, and every other surface that shows it masks it.
// This path is new, and it publishes state to four places at once — the destroy
// reply, the MCP result, the WARN line and the archived `status_details` — so an
// unmasked read here would be the single surface that leaks what the others hide,
// including into a compliance archive that outlives the incarnation.
//
// The SIDs in the same record are deliberately NOT masked: that list comes from
// `incarnation_membership`, which is keeper-owned and holds validated FQDNs.
func TestDeleteAfterTeardown_ForceMasksStateDerivedValues(t *testing.T) {
	const secretRef = "vault:secret/clouds/aws#access_key"
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	tx.selectRow = scriptedRow{values: []any{[]byte(
		`{"provisioned_provider":"` + secretRef + `","provisioned_vm_ids":["` + secretRef + `"]}`)}}
	tx.rowsResult = &fakeRows{rows: []staticRow{{values: []any{"vm-1.example.com"}}}}
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	res, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil)
	if err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if res.Unreleased == nil {
		t.Fatal("Unreleased = nil")
	}
	if res.Unreleased.Provider != audit.MaskedValue {
		t.Errorf("returned provider = %q, want it masked — this value goes to the destroy reply, "+
			"the MCP result and the WARN line", res.Unreleased.Provider)
	}
	if len(res.Unreleased.VMIDs) != 1 || res.Unreleased.VMIDs[0] != audit.MaskedValue {
		t.Errorf("returned vm_ids = %q, want them masked", res.Unreleased.VMIDs)
	}
	// `status_details` is the durable copy of THIS record: an unmasked ref there
	// survives the incarnation by the whole retention window. (The neighbouring
	// `state` column is archived verbatim by design — a compliance archive keeps
	// the raw row — so this assertion is about the derived record, not about the
	// archive as a whole.)
	_, patch := archiveInsertArgs(t, tx)
	if strings.Contains(string(patch), secretRef) {
		t.Errorf("archived status_details carries the raw vault ref: %s", patch)
	}
	// Masking must not swallow the record: the SIDs still name the abandoned host.
	if !strings.Contains(string(patch), "vm-1.example.com") {
		t.Errorf("archived status_details = %s, want it to still name the member host", patch)
	}
}

// TestDeleteAfterTeardown_ForceAuditPayloadOmitsEmptyDimensions — GUARD: the
// audit payload names only the dimensions that actually hold something, exactly
// like the reply and the archived `status_details` do.
//
// The audit writer re-masks the payload by walking it with reflect, and that walk
// turns a struct into a map by json-tag name while ignoring `omitempty`. Handing
// it the struct is therefore not equivalent to handing it the marshalled form:
// the surface built to outlive the response would be the one answering
// `"vm_ids": []` where every other surface omits the key. That is the
// "checked-clean or could-not-tell" ambiguity this whole object exists to avoid,
// reappearing one level down — and it appears on the ONE surface an operator
// reaches for after the response is gone, since `incarnation_archive` has no read
// API.
func TestDeleteAfterTeardown_ForceAuditPayloadOmitsEmptyDimensions(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	// Hosts only: no cloud was ever provisioned, so provider and vm_ids have
	// nothing to say — and must therefore say nothing.
	tx.selectRow = scriptedRow{values: []any{[]byte(`{}`)}}
	tx.rowsResult = &fakeRows{rows: []staticRow{{values: []any{"vm-1.example.com"}}}}
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	if _, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil); err != nil {
		t.Fatalf("DeleteAfterTeardown: %v", err)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	raw, ok := aw.events[0].Payload["unreleased"]
	if !ok {
		t.Fatal("audit payload has no `unreleased` key — this force abandoned a host")
	}
	got, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("audit payload `unreleased` is %T, want a map — a struct here reaches the "+
			"re-masking reflect walk, which materializes every field regardless of omitempty", raw)
	}
	for _, empty := range []string{"provider", "vm_ids"} {
		if v, present := got[empty]; present {
			t.Errorf("audit payload names %q as %#v; nothing was provisioned, so the key must be "+
				"absent — an empty value here reads as \"checked, found none\" on the one "+
				"surface that outlives the response", empty, v)
		}
	}
	if _, present := got["sids"]; !present {
		t.Errorf("audit payload dropped `sids` = %#v — the abandoned host is the whole finding", got)
	}
}

// TestDeleteAfterTeardown_ForceRosterErrorRefusesToDelete — GUARD: if the roster
// cannot be read, the destroy fails rather than deleting the row with a short
// list. Swallowing the error is the worse of the two failures NIM-395 is about:
// on an incarnation whose only abandoned resource is its hosts it produces an
// empty record, which the follow-up then legitimately drops — force reports a
// clean walk-away over hosts it never checked. Nothing is archived or deleted,
// because the collection runs before the first Exec.
func TestDeleteAfterTeardown_ForceRosterErrorRefusesToDelete(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 1"))
	tx.selectRow = scriptedRow{values: []any{[]byte(provisionedStateJSON)}}
	tx.rowsResult = &fakeRows{err: errors.New("roster boom")}
	pool := &fakePool{txs: []*fakeTx{tx}}
	aw := &fakeAuditWriter{}

	if _, err := DeleteAfterTeardown(context.Background(), pool, aw, "redis-prod", true, nil); err == nil {
		t.Fatal("roster unreadable and the destroy still succeeded — the record is gone and " +
			"nothing says which hosts it held")
	}
	if tx.committed {
		t.Error("tx committed despite an unreadable roster")
	}
	if tx.execN != 0 {
		t.Errorf("Exec calls = %d, want 0 — nothing may be archived or deleted "+
			"before the roster is known", tx.execN)
	}
	if len(aw.events) != 0 {
		t.Errorf("audit events = %d, want 0 — no destroy happened", len(aw.events))
	}
}

// TestDeleteAfterTeardown_ForceStateRowGoneDegrades — the row can leave
// `destroying` between the caller's check and this collection (a concurrent
// unlock, a competing destroy). That is not an error: the DELETE below is
// guarded on the same status and turns into the documented no-op, so the
// collection reports "nothing seen" and lets it get there.
func TestDeleteAfterTeardown_ForceStateRowGoneDegrades(t *testing.T) {
	tx := deleteTx(pgconn.NewCommandTag("DELETE 0"))
	tx.selectRow = scriptedRow{err: pgx.ErrNoRows}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := DeleteAfterTeardown(context.Background(), pool, &fakeAuditWriter{}, "redis-prod", true, nil)
	if err != nil {
		t.Fatalf("a vanished row must not fail the destroy: %v", err)
	}
	if res.Deleted {
		t.Error("Deleted = true on a DELETE that matched no row")
	}
	if res.Unreleased != nil {
		t.Errorf("Unreleased = %+v, want nil — nothing was observed to abandon", res.Unreleased)
	}
}

// TestJSONStringSlice — state is authored by the service, so a wrong shape must
// degrade to "no cloud coordinates", never fail the destroy.
func TestJSONStringSlice(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"strings", []any{"a", "b"}, "a,b"},
		{"mixed drops non-strings", []any{"a", 42, nil, "b"}, "a,b"},
		{"drops empty strings", []any{"", "a"}, "a"},
		{"not an array", "i-aaa111", ""},
		{"absent key", nil, ""},
		{"empty array", []any{}, ""},
		{"all non-strings", []any{1, 2}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(jsonStringSlice(tc.in), ","); got != tc.want {
				t.Errorf("jsonStringSlice(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
