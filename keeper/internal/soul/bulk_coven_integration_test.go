//go:build integration

// Integration tests for bulk coven-assign (BulkAssignCoven / CountBulkMatched)
// against real Postgres (keyset chunking, idempotency, scope-intersection can't
// be verified on a fake pool — they're SQL-driven). Use the shared
// integration_test.go harness (integrationPool / resetAll).

package soul

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedBulkSoul inserts a host with the given coven set (status pending).
func seedBulkSoul(t *testing.T, sid string, coven []string) {
	t.Helper()
	s := &Soul{SID: sid, Status: StatusPending, Coven: coven}
	if err := Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedBulkSoul(%s): %v", sid, err)
	}
}

func covenOf(t *testing.T, sid string) []string {
	t.Helper()
	got, err := SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID(%s): %v", sid, err)
	}
	out := append([]string(nil), got.Coven...)
	sort.Strings(out)
	return out
}

// seedIncarnationRow / seedMembership seed an incarnation and bind SIDs to it
// (NIM-124: membership is the `incarnation_membership` relation, no longer
// incarnation.name in souls.coven[]). Raw SQL is used because the incarnation
// package imports soul — importing it here would be an import cycle.
func seedIncarnationRow(t *testing.T, name string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation (name, service, service_version, status)
		 VALUES ($1, 'redis', 'v1.0.0', 'ready')`, name); err != nil {
		t.Fatalf("seedIncarnationRow(%s): %v", name, err)
	}
}

func seedMembership(t *testing.T, incName string, sids ...string) {
	t.Helper()
	for _, sid := range sids {
		if _, err := integrationPool.Exec(context.Background(),
			`INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ($1, $2)`,
			incName, sid); err != nil {
			t.Fatalf("seedMembership(%s, %s): %v", incName, sid, err)
		}
	}
}

func TestIntegration_Bulk_Append_All_Idempotent(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev", "edge"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Status != BulkCompleted {
		t.Errorf("status = %q, want completed", rep.Status)
	}
	if rep.Matched != 2 {
		t.Errorf("matched = %d, want 2", rep.Matched)
	}
	// b already has edge → idempotent filtering → only a changes.
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (b already has edge)", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev", "edge"}) {
		t.Errorf("a.coven = %v, want [dev edge]", got)
	}

	// Repeat call = no-op (both already have edge).
	rep2, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idempotent)", rep2.Changed)
	}
}

func TestIntegration_Bulk_Remove_Idempotent(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "old"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "old", CovenRemove)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (only a has old)", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("a.coven = %v, want [dev]", got)
	}

	rep2, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "old", CovenRemove)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0", rep2.Changed)
	}
}

// TestIntegration_Bulk_Chunking — >bulkChunkSize hosts: keyset iteration must
// walk all rows across multiple chunks without skips/dupes.
func TestIntegration_Bulk_Chunking(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	const n = bulkChunkSize + 137 // > 1 chunk
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != n {
		t.Errorf("matched = %d, want %d", rep.Matched, n)
	}
	if rep.Changed != n {
		t.Errorf("changed = %d, want %d (no host had batch)", rep.Changed, n)
	}
	if rep.ChunksCommitted < 2 {
		t.Errorf("chunksCommitted = %d, want >= 2 (>1 chunk)", rep.ChunksCommitted)
	}

	// Sanity check: all hosts got the label.
	var withBatch int
	if err := integrationPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("count: %v", err)
	}
	if withBatch != n {
		t.Errorf("hosts with 'batch' = %d, want %d (keyset skipped/dup rows)", withBatch, n)
	}
}

func TestIntegration_Bulk_DryRun_NoWrite(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	matched, err := CountBulkMatched(ctx, integrationPool, sel, scope)
	if err != nil {
		t.Fatalf("CountBulkMatched: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	// dry_run must write nothing.
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("a.coven mutated by dry_run: %v", got)
	}
}

func TestIntegration_Bulk_Selector_SIDs(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})
	seedBulkSoul(t, "c.example.com", []string{"dev"})

	sel := BulkSelector{SIDs: []string{"a.example.com", "c.example.com"}}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "b.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("b mutated though not in sids: %v", got)
	}
}

func TestIntegration_Bulk_Selector_Status(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	kid := "kid-1"
	if err := UpdateStatus(ctx, integrationPool, "a.example.com", StatusConnected, &kid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	seedBulkSoul(t, "b.example.com", []string{"dev"}) // stays pending

	sel := BulkSelector{All: true, Status: StatusConnected}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1", rep.Matched, rep.Changed)
	}
}

func TestIntegration_Bulk_EmptySelector(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	_, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{}, BulkScope{Unrestricted: true}, "edge", CovenAppend)
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Errorf("err = %v, want ErrBulkEmptySelector", err)
	}
}

// --- scope-intersection (CRITICAL) ---

// TestIntegration_Bulk_Scope_HostsSubset — gate (a): target hosts ⊆ scope. An
// operator with coven=dev doesn't touch hosts outside dev even with all=true.
func TestIntegration_Bulk_Scope_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}} // Unrestricted=false

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	// Only dev-host falls under scope; prod-host is excluded by the
	// coven && ARRAY['dev'] predicate. append label 'dev' ∈ scope — allowed.
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod-host out of scope)", rep.Matched)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// TestIntegration_Bulk_Scope_HostsSubset_ViaSIDs — gate (a) can't be bypassed
// via an explicit sids list: a host outside scope still doesn't reach the UPDATE.
func TestIntegration_Bulk_Scope_HostsSubset_ViaSIDs(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	sel := BulkSelector{SIDs: []string{"prod-host.example.com"}}
	scope := BulkScope{Covens: []string{"dev"}}

	// label 'dev' ∈ scope (passes gate b), but the target host is outside scope.
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 0 || rep.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0 (host out of scope)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("out-of-scope host mutated: %v", got)
	}
}

// TestIntegration_Bulk_Scope_LabelOutOfScope — gate (b): can't attach a label
// outside scope (privilege escalation onto someone else's target).
func TestIntegration_Bulk_Scope_LabelOutOfScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}}

	// An operator with coven=dev tries to attach 'prod' — rejected BEFORE UPDATE.
	_, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "prod", CovenAppend)
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if got := covenOf(t, "dev-host.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("host mutated despite out-of-scope label: %v", got)
	}
}

// TestIntegration_Bulk_Scope_Remove_HostsSubset — removing a label outside
// scope doesn't expand the target (removal isn't escalation); gate (b) doesn't
// apply to remove, but gate (a) still restricts target hosts by scope.
func TestIntegration_Bulk_Scope_Remove_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev", "shared"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod", "shared"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "shared", CovenRemove)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	// Only dev-host is in scope → shared is removed only from it.
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (prod-host out of scope)", rep.Changed)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod", "shared"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// --- partial semantics (no-rollback path) ---

// cancelAfterChunkPool wraps a real *pgxpool.Pool and cancels the shared
// cancellable context BEFORE the K-th BeginTx. Chunks 1..K-1 commit normally,
// then on the K-th BeginTx(ctx) pgx returns context.Canceled → bulkUpdateChunk
// fails → BulkAssignCoven returns BulkPartial without rolling back
// already-committed chunks. Test-only seam: production code stays untouched,
// failure injection goes through cancelling the passed context rather than a
// hook in BulkAssignCoven.
//
// CountBulkMatched (the first round-trip in BulkAssignCoven) goes through
// QueryRow, not BeginTx, so the BeginTx counter maps directly to the chunk
// number.
type cancelAfterChunkPool struct {
	*pgxpool.Pool
	cancel   context.CancelFunc
	failOn   int // chunk number (1-based) whose BeginTx cancels ctx.
	beginCnt int
}

func (p *cancelAfterChunkPool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	p.beginCnt++
	if p.beginCnt == p.failOn {
		// Cancel BEFORE delegating to the real pool: BeginTx with a cancelled ctx
		// returns an error, so this chunk (K) won't commit.
		p.cancel()
	}
	return p.Pool.BeginTx(ctx, opts)
}

// TestIntegration_Bulk_Partial_NoRollback — chunk K fails midway through a
// large selection: chunks 1..K-1 are committed (label on their rows), returns
// partial with Err and changed < matched; an idempotent retry finishes the
// remainder without dupes.
func TestIntegration_Bulk_Partial_NoRollback(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3 full chunks + tail: enough to fail on chunk 2 and leave a non-empty
	// remainder (chunks 2..3 + tail) for the follow-up retry.
	const n = bulkChunkSize*3 + 91
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	const failChunk = 2 // fail on chunk 2's BeginTx → exactly 1 committed.
	pool := &cancelAfterChunkPool{Pool: integrationPool, cancel: cancel, failOn: failChunk}

	rep, err := BulkAssignCoven(ctx, pool, sel, scope, "batch", CovenAppend)
	if err == nil {
		t.Fatal("BulkAssignCoven: err = nil, want partial-failure error")
	}
	if rep.Status != BulkPartial {
		t.Errorf("status = %q, want partial", rep.Status)
	}
	if rep.Err == nil {
		t.Errorf("rep.Err = nil, want non-nil on partial")
	}
	if rep.Matched != n {
		t.Errorf("matched = %d, want %d (count before iteration)", rep.Matched, n)
	}
	if rep.ChunksCommitted != failChunk-1 {
		t.Errorf("chunksCommitted = %d, want %d", rep.ChunksCommitted, failChunk-1)
	}
	if rep.Changed >= rep.Matched {
		t.Errorf("changed = %d, want < matched (%d) on partial", rep.Changed, rep.Matched)
	}
	if rep.Changed != bulkChunkSize {
		t.Errorf("changed = %d, want %d (exactly 1 committed chunk)", rep.Changed, bulkChunkSize)
	}

	// Chunks 1..K-1 are ACTUALLY committed in real PG (label present on rows).
	var withBatch int
	if err := integrationPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("count after partial: %v", err)
	}
	if withBatch != bulkChunkSize {
		t.Errorf("hosts with 'batch' after partial = %d, want %d (commit of chunk 1 survived)", withBatch, bulkChunkSize)
	}

	// Idempotent retry FINISHES the remainder (fresh, non-cancelled ctx).
	rep2, err := BulkAssignCoven(context.Background(), integrationPool, sel, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven repeat: %v", err)
	}
	if rep2.Status != BulkCompleted {
		t.Errorf("repeat status = %q, want completed", rep2.Status)
	}
	// Retry touches only not-yet-labeled rows (idem-filtering): n - already-labeled.
	if rep2.Changed != n-bulkChunkSize {
		t.Errorf("repeat changed = %d, want %d (remainder only)", rep2.Changed, n-bulkChunkSize)
	}

	// Final state is consistent: label present on exactly all n, no dupes in the array.
	if err := integrationPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if withBatch != n {
		t.Errorf("final hosts with 'batch' = %d, want %d", withBatch, n)
	}
	var dupHosts int
	if err := integrationPool.QueryRow(context.Background(),
		// cardinality(array) > size of the unique set → duplicate label in the array.
		`SELECT COUNT(*) FROM souls
		 WHERE cardinality(coven) <> cardinality(ARRAY(SELECT DISTINCT unnest(coven)))`).Scan(&dupHosts); err != nil {
		t.Fatalf("dup-check: %v", err)
	}
	if dupHosts != 0 {
		t.Errorf("hosts with duplicate labels in coven = %d, want 0 (idempotent filtering prevented double append)", dupHosts)
	}
}

// TestIntegration_Bulk_ExactMultipleOfChunk — number of matching rows is
// EXACTLY a multiple of bulkChunkSize: checks for an off-by-one in keyset
// iteration. The last keyset window is exactly full (scanned == chunk), so
// iteration makes one more pass with an empty window and exits cleanly — no
// extra changes/panic.
func TestIntegration_Bulk_ExactMultipleOfChunk(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	const n = bulkChunkSize * 2 // exactly 2 full chunks.
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "exact", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != n || rep.Changed != n {
		t.Errorf("matched/changed = %d/%d, want %d/%d", rep.Matched, rep.Changed, n, n)
	}
	// 2 full chunks → chunk 2 has scanned == chunk, so the loop makes a 3rd
	// pass with an empty window (scanned 0 < chunk → exit). ChunksCommitted
	// also counts the empty final pass (it commits a no-op transaction).
	if rep.ChunksCommitted != 3 {
		t.Errorf("chunksCommitted = %d, want 3 (2 full + an empty final one)", rep.ChunksCommitted)
	}

	var withExact int
	if err := integrationPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM souls WHERE 'exact' = ANY(coven)").Scan(&withExact); err != nil {
		t.Fatalf("count: %v", err)
	}
	if withExact != n {
		t.Errorf("hosts with 'exact' = %d, want %d (off-by-one in keyset)", withExact, n)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- mode=replace integration ---

// TestIntegration_Bulk_Replace_MutatesSet — replace sets coven to exactly the
// given set, dropping existing labels.
func TestIntegration_Bulk_Replace_MutatesSet(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "old"})
	seedBulkSoul(t, "b.example.com", []string{"stage"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkReplaceCoven(ctx, integrationPool, sel, scope, []string{"prod", "edge"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	if rep.Status != BulkCompleted {
		t.Errorf("status = %q, want completed", rep.Status)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"edge", "prod"}) {
		t.Errorf("a.coven = %v, want [edge prod]", got)
	}
	if got := covenOf(t, "b.example.com"); !equalStr(got, []string{"edge", "prod"}) {
		t.Errorf("b.coven = %v, want [edge prod]", got)
	}

	// Idem: repeat with the same set → 0 changed (coven IS DISTINCT FROM
	// $labels = false on every host).
	rep2, err := BulkReplaceCoven(ctx, integrationPool, sel, scope, []string{"prod", "edge"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idem-set-equal)", rep2.Changed)
	}
}

// TestIntegration_Bulk_Replace_EmptySet_ClearsAll — replace with an empty set
// clears coven for hosts under the selector.
func TestIntegration_Bulk_Replace_EmptySet_ClearsAll(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "edge"})

	rep, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, []string{})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); len(got) != 0 {
		t.Errorf("a.coven = %v, want []", got)
	}
}

// TestIntegration_Bulk_Replace_ScopeIntersection — gate (a) on replace: hosts
// outside scope don't reach the UPDATE even with all=true.
func TestIntegration_Bulk_Replace_ScopeIntersection(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	rep, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, []string{"dev"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	// Only dev-host falls under scope; the set {dev} equals current {dev} →
	// idem-filtering → 0 changed, but matched=1.
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod-host out of scope)", rep.Matched)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// TestIntegration_Bulk_Replace_LabelOutOfScope — gate (b) on replace: a set
// containing a label outside scope is rejected BEFORE UPDATE.
func TestIntegration_Bulk_Replace_LabelOutOfScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})

	_, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, []string{"dev", "prod"})
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if got := covenOf(t, "dev-host.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("host mutated despite out-of-scope label set: %v", got)
	}
}

// --- selector.incarnation integration ---

// TestIntegration_Bulk_Selector_Incarnation_Match — bulk by incarnation matches
// its members via `incarnation_membership` (NIM-124), not incarnation-name in coven.
func TestIntegration_Bulk_Selector_Incarnation_Match(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// NIM-124: membership via incarnation_membership, not incarnation-name in coven.
	seedIncarnationRow(t, "redis")
	seedBulkSoul(t, "r1.example.com", nil)
	seedBulkSoul(t, "r2.example.com", []string{"dc-eu"})
	seedBulkSoul(t, "other.example.com", []string{"nginx"})
	seedMembership(t, "redis", "r1.example.com", "r2.example.com")

	sel := BulkSelector{Incarnation: "redis"}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "patched", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2 (redis hosts only)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "other.example.com"); !equalStr(got, []string{"nginx"}) {
		t.Errorf("other mutated though not in incarnation: %v", got)
	}
}

// TestIntegration_Bulk_Selector_Incarnation_NoMatch — nonexistent incarnation
// → 0 matched/changed.
func TestIntegration_Bulk_Selector_Incarnation_NoMatch(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "r1.example.com", []string{"redis"})

	sel := BulkSelector{Incarnation: "ghost-incarnation"}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, BulkScope{Unrestricted: true},
		"patched", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 0 || rep.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0", rep.Matched, rep.Changed)
	}
}

// TestIntegration_Bulk_Selector_Incarnation_ScopeIntersection — incarnation
// + scope (a): scope still trims target hosts.
func TestIntegration_Bulk_Selector_Incarnation_ScopeIntersection(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// redis-incarnation covers both hosts, but prod-host is OUTSIDE scope=dev.
	// NIM-124: membership via incarnation_membership; coven holds only real tags.
	seedIncarnationRow(t, "redis")
	seedBulkSoul(t, "redis-dev.example.com", []string{"dev"})
	seedBulkSoul(t, "redis-prod.example.com", []string{"prod"})
	seedMembership(t, "redis", "redis-dev.example.com", "redis-prod.example.com")

	sel := BulkSelector{Incarnation: "redis"}
	scope := BulkScope{Covens: []string{"dev"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (only redis-dev in scope)", rep.Matched)
	}
	if got := covenOf(t, "redis-prod.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("redis-prod mutated despite out-of-scope: %v", got)
	}
}
