//go:build integration

// Integration tests for souls CRUD via testcontainers-go (postgres:16-alpine).
// Pattern matches keeper/internal/operator/integration_test.go.

package soul

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("soul integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("soul integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("soul integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("soul integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("soul integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	// CASCADE: soul_seeds → souls → operators (FK chain). incarnation +
	// incarnation_membership (NIM-124) for the Incarnation-selector bulk tests.
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, incarnation_membership,
		 incarnation, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func TestIntegration_Insert_Pending_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	requestedAt := time.Now().UTC()
	s := &Soul{
		SID:          "host1.example.com",
		Status:       StatusPending,
		Coven:        []string{"prod", "db"},
		CreatedByAID: &creator,
		RequestedAt:  &requestedAt,
		Note:         "test bootstrap",
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.RegisteredAt.IsZero() {
		t.Errorf("RegisteredAt not populated")
	}

	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.SID != "host1.example.com" || got.Status != StatusPending {
		t.Errorf("got = %+v", got)
	}
	if len(got.Coven) != 2 || got.Coven[0] != "prod" {
		t.Errorf("Coven = %v, want [prod db]", got.Coven)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", got.CreatedByAID)
	}
	if got.Note != "test bootstrap" {
		t.Errorf("Note = %q", got.Note)
	}
}

// TestIntegration_Insert_Traits_RoundTrip — GUARD (ADR-060): operator-set
// traits (scalar + list) are serialized into the `souls.traits` jsonb column
// on Insert and read back as the same value via SelectBySID (round-trip). A
// host without traits reads back as an empty map (jsonb '{}' NOT NULL
// DEFAULT), not a nil panic.
func TestIntegration_Insert_Traits_RoundTrip(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	s := &Soul{
		SID:    "host1.example.com",
		Status: StatusPending,
		Coven:  []string{"prod"},
		Traits: map[string]any{
			"namespace": "dba-ns",              // scalar
			"product":   "aboba",               // scalar
			"owners":    []any{"alice", "bob"}, // list
		},
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Traits == nil {
		t.Fatalf("Traits = nil after round-trip, want map")
	}
	if got.Traits["namespace"] != "dba-ns" {
		t.Errorf("Traits[namespace] = %v, want dba-ns", got.Traits["namespace"])
	}
	if got.Traits["product"] != "aboba" {
		t.Errorf("Traits[product] = %v, want aboba", got.Traits["product"])
	}
	// Array JSON-deserializes to []any.
	owners, ok := got.Traits["owners"].([]any)
	if !ok || len(owners) != 2 || owners[0] != "alice" || owners[1] != "bob" {
		t.Errorf("Traits[owners] = %v, want [alice bob]", got.Traits["owners"])
	}

	// Coven is unaffected by the parallel traits axis.
	if len(got.Coven) != 1 || got.Coven[0] != "prod" {
		t.Errorf("Coven = %v, want [prod] (traits are a separate axis)", got.Coven)
	}

	// Host without traits → empty map (jsonb '{}'), not nil.
	bare := &Soul{SID: "host2.example.com", Status: StatusPending}
	if err := Insert(ctx, integrationPool, bare); err != nil {
		t.Fatalf("Insert(bare): %v", err)
	}
	gotBare, err := SelectBySID(ctx, integrationPool, "host2.example.com")
	if err != nil {
		t.Fatalf("SelectBySID(bare): %v", err)
	}
	if gotBare.Traits == nil || len(gotBare.Traits) != 0 {
		t.Errorf("bare Traits = %v, want empty map", gotBare.Traits)
	}
}

func TestIntegration_Insert_RejectsDuplicateSID(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "host1.example.com"}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	dup := &Soul{SID: "host1.example.com"}
	err := Insert(ctx, integrationPool, dup)
	if !errors.Is(err, ErrSoulAlreadyExists) {
		t.Errorf("err = %v, want ErrSoulAlreadyExists", err)
	}
}

func TestIntegration_Insert_RejectsBadSIDViaCHECK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// Uppercase SID: rejected by both ValidSID (Go) and the PG CHECK — proves
	// Insert errors regardless of which layer catches it first.
	s := &Soul{SID: "HOST.EXAMPLE.COM"}
	if err := Insert(ctx, integrationPool, s); err == nil {
		t.Fatal("Insert with uppercase SID returned nil err")
	}
}

func TestIntegration_UpdateStatus(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "host1.example.com", Status: StatusPending}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	kid := "kid-1"
	if err := UpdateStatus(ctx, integrationPool, "host1.example.com", StatusConnected, &kid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != StatusConnected {
		t.Errorf("Status = %q, want connected", got.Status)
	}
	if got.LastSeenByKID == nil || *got.LastSeenByKID != "kid-1" {
		t.Errorf("LastSeenByKID = %v, want kid-1", got.LastSeenByKID)
	}
}

func TestIntegration_UpdateStatus_NotFound(t *testing.T) {
	resetAll(t)
	err := UpdateStatus(context.Background(), integrationPool, "missing.host.example.com", StatusConnected, nil)
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestIntegration_SelectAll_FilterAndPaginate(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	for i, host := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		s := &Soul{
			SID:    host,
			Status: StatusPending,
			Coven:  []string{"prod"},
		}
		if i == 0 {
			s.Status = StatusConnected
		}
		if err := Insert(ctx, integrationPool, s); err != nil {
			t.Fatalf("Insert(%s): %v", host, err)
		}
	}

	unrestricted := ListScope{Unrestricted: true}

	all, total, err := SelectAll(ctx, integrationPool, ListFilter{}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Errorf("total/len = %d/%d, want 3/3", total, len(all))
	}

	pending, totalPending, err := SelectAll(ctx, integrationPool, ListFilter{Status: StatusPending}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(pending): %v", err)
	}
	if totalPending != 2 || len(pending) != 2 {
		t.Errorf("pending total/len = %d/%d, want 2/2", totalPending, len(pending))
	}

	byCoven, _, err := SelectAll(ctx, integrationPool, ListFilter{Coven: "prod"}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven): %v", err)
	}
	if len(byCoven) != 3 {
		t.Errorf("by-coven len = %d, want 3", len(byCoven))
	}
}

// TestIntegration_SelectAll_Scope — scope-pushdown `coven && ARRAY[scope]`
// (ADR-047 S3b) on a real PG: a coven-scoped operator sees ONLY hosts in
// their own covens; an empty scope (fail-closed) → zero rows (NOT the whole
// fleet); filter ∩ scope is AND. total is coherent with the result set
// (COUNT with the same scope-WHERE).
func TestIntegration_SelectAll_Scope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seed := []struct {
		sid   string
		coven []string
	}{
		{"prod-1.example.com", []string{"prod"}},
		{"prod-2.example.com", []string{"prod", "dc-eu"}},
		{"stg-1.example.com", []string{"staging"}},
		{"dev-1.example.com", []string{"dev"}},
	}
	for _, s := range seed {
		if err := Insert(ctx, integrationPool, &Soul{SID: s.sid, Status: StatusPending, Coven: s.coven}); err != nil {
			t.Fatalf("Insert(%s): %v", s.sid, err)
		}
	}

	// scope=[prod] → only prod hosts (a subset, not the whole fleet).
	prodItems, prodTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: []string{"prod"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=prod): %v", err)
	}
	if prodTotal != 2 || len(prodItems) != 2 {
		t.Errorf("scope=prod total/len = %d/%d, want 2/2 (prod-1, prod-2)", prodTotal, len(prodItems))
	}

	// scope=[prod,staging] → union.
	union, unionTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: []string{"prod", "staging"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=prod,staging): %v", err)
	}
	if unionTotal != 3 || len(union) != 3 {
		t.Errorf("scope=[prod,staging] total/len = %d/%d, want 3/3", unionTotal, len(union))
	}

	// fail-closed: an empty scope (not Unrestricted) → zero rows, NOT the whole fleet.
	empty, emptyTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: nil}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=empty): %v", err)
	}
	if emptyTotal != 0 || len(empty) != 0 {
		t.Errorf("empty scope total/len = %d/%d, want 0/0 (fail-closed, NOT the entire fleet)", emptyTotal, len(empty))
	}

	// filter ∩ scope: an operator with scope=[prod] filtering coven=staging → empty
	// (staging is outside their scope, AND-intersection, not an expansion).
	cross, crossTotal, err := SelectAll(ctx, integrationPool, ListFilter{Coven: "staging"}, ListScope{Covens: []string{"prod"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(filter=staging ∩ scope=prod): %v", err)
	}
	if crossTotal != 0 || len(cross) != 0 {
		t.Errorf("filter=staging cap scope=prod total/len = %d/%d, want 0/0 (scope is not widened by filter)", crossTotal, len(cross))
	}
}

// TestIntegration_ListForScopeEval_KeysetNoDupesNoGaps — the MAIN keyset test
// (ADR-047 S3b-2a): a fleet with a BATCH of identical registered_at, page_size
// smaller than the fleet → walking all pages with the composite cursor
// `(registered_at, sid)` collects EVERY host EXACTLY ONCE (no dupes, no gaps
// at page boundaries). A bare sid cursor would leave holes on equal
// registered_at — this test catches that regression.
func TestIntegration_ListForScopeEval_KeysetNoDupesNoGaps(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// 7 hosts with the SAME registered_at (a batch), 2 with different times.
	// The batch forces the composite cursor to break ties by sid.
	sameTS := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	earlier := sameTS.Add(-time.Hour)
	later := sameTS.Add(time.Hour)
	seed := []struct {
		sid string
		at  time.Time
	}{
		{"h1.example.com", sameTS}, {"h2.example.com", sameTS}, {"h3.example.com", sameTS},
		{"h4.example.com", sameTS}, {"h5.example.com", sameTS}, {"h6.example.com", sameTS},
		{"h7.example.com", sameTS},
		{"early.example.com", earlier},
		{"late.example.com", later},
	}
	want := map[string]struct{}{}
	for _, s := range seed {
		at := s.at
		if err := Insert(ctx, integrationPool, &Soul{SID: s.sid, Status: StatusPending, RegisteredAt: at}); err != nil {
			t.Fatalf("Insert(%s): %v", s.sid, err)
		}
		want[s.sid] = struct{}{}
	}

	const pageSize = 3 // smaller than the fleet (9) and the batch (7) → many boundaries.
	got := map[string]struct{}{}
	var cursor *KeysetCursorBound
	pages := 0
	for {
		rows, err := ListForScopeEval(ctx, integrationPool, ListFilter{}, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListForScopeEval (page %d): %v", pages, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if _, dup := got[r.SID]; dup {
				t.Fatalf("DUPLICATE: %s encountered twice (keyset boundary leaked)", r.SID)
			}
			got[r.SID] = struct{}{}
		}
		last := rows[len(rows)-1]
		cursor = &KeysetCursorBound{RegisteredAt: last.RegisteredAt, SID: last.SID}
		pages++
		if len(rows) < pageSize {
			break
		}
		if pages > 20 {
			t.Fatal("cursor does not converge (>20 pages for 9 hosts)")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d hosts, want %d - there are gaps/duplicates", len(got), len(want))
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			t.Errorf("host %s skipped by keyset traversal", sid)
		}
	}
}

// TestIntegration_ListForScopeEval_OrderStable — the registered_at DESC, sid
// ASC ordering is stable: the cursor walk is strictly monotonic (each next
// page never "goes backward").
func TestIntegration_ListForScopeEval_OrderStable(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, sid := range []string{"c.example.com", "a.example.com", "b.example.com"} {
		// a,b in the same second, c later: checks both the tie-break and DESC.
		at := base
		if sid == "c.example.com" {
			at = base.Add(time.Minute)
		}
		if err := Insert(ctx, integrationPool, &Soul{SID: sid, Status: StatusPending, RegisteredAt: at}); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}
	rows, err := ListForScopeEval(ctx, integrationPool, ListFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListForScopeEval: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// c (later) comes first; then a,b (equal TS) ordered by sid ASC.
	wantOrder := []string{"c.example.com", "a.example.com", "b.example.com"}
	for i, w := range wantOrder {
		if rows[i].SID != w {
			t.Errorf("rows[%d] = %s, want %s (order registered_at DESC, sid ASC violated)", i, rows[i].SID, w)
		}
	}
}

// TestIntegration_ListForScopeEval_FilterPushdown — the user filter
// (status/transport/coven) is applied as a SQL WHERE inside the keyset window
// (ADR-047 S3b-2a fix). Without pushdown, keyset mode would return all rows
// and the handler's Go-eval couldn't distinguish "filtered out" from "outside
// scope". This CRUD-level test pins down that the filter cuts the selection
// IN the DB rather than being ignored.
func TestIntegration_ListForScopeEval_FilterPushdown(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seed := []*Soul{
		{SID: "web-01.example.com", Transport: TransportAgent, Status: StatusConnected, Coven: []string{"prod"}, RegisteredAt: base},
		{SID: "web-02.example.com", Transport: TransportAgent, Status: StatusPending, Coven: []string{"dev"}, RegisteredAt: base.Add(-time.Second)},
		{SID: "ssh-03.example.com", Transport: TransportSSH, Status: StatusConnected, Coven: []string{"prod"}, RegisteredAt: base.Add(-2 * time.Second)},
	}
	for _, s := range seed {
		if err := Insert(ctx, integrationPool, s); err != nil {
			t.Fatalf("Insert(%s): %v", s.SID, err)
		}
	}

	collect := func(f ListFilter) map[string]struct{} {
		rows, err := ListForScopeEval(ctx, integrationPool, f, nil, 100)
		if err != nil {
			t.Fatalf("ListForScopeEval(%+v): %v", f, err)
		}
		out := map[string]struct{}{}
		for _, r := range rows {
			out[r.SID] = struct{}{}
		}
		return out
	}

	// status=connected → web-01, ssh-03 (NOT web-02 pending).
	got := collect(ListFilter{Status: StatusConnected})
	if len(got) != 2 {
		t.Errorf("status=connected: got %d, want 2 (%v)", len(got), got)
	}
	if _, ok := got["web-02.example.com"]; ok {
		t.Error("status=connected let through pending web-02 - filter not applied in SQL")
	}
	// transport=ssh → only ssh-03.
	got = collect(ListFilter{Transport: TransportSSH})
	if len(got) != 1 {
		t.Errorf("transport=ssh: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["ssh-03.example.com"]; !ok {
		t.Error("transport=ssh did not return ssh-03")
	}
	// coven=dev → only web-02.
	got = collect(ListFilter{Coven: "dev"})
	if len(got) != 1 {
		t.Errorf("coven=dev: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["web-02.example.com"]; !ok {
		t.Error("coven=dev did not return web-02")
	}
	// Combined (AND): connected + prod + agent → only web-01.
	got = collect(ListFilter{Status: StatusConnected, Coven: "prod", Transport: TransportAgent})
	if len(got) != 1 {
		t.Errorf("combined: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["web-01.example.com"]; !ok {
		t.Error("combined filter did not return web-01")
	}
}

func TestIntegration_UpdateStatus_DestroyedAcceptedByCHECK(t *testing.T) {
	// ADR-017 cascade: migration 016 extended the `souls_status_valid` CHECK
	// with the `destroyed` value. Verifies PG accepts the new enum.
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "destroyed-host.example.com"}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "destroyed-host.example.com", StatusDestroyed, nil); err != nil {
		t.Fatalf("UpdateStatus(destroyed): %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "destroyed-host.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != StatusDestroyed {
		t.Errorf("status = %q, want destroyed", got.Status)
	}
}

func TestIntegration_FKToOperators_SetNullOnDelete(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice")
	creator := "archon-alice"
	s := &Soul{SID: "host1.example.com", CreatedByAID: &creator}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Delete the operator — souls.created_by_aid must become NULL (ON DELETE SET NULL).
	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = $1`, "archon-alice"); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v, want nil after operator delete", got.CreatedByAID)
	}
}

// TestIntegration_SelectStats_Scope — the MAIN guard for the GET
// /v1/souls/stats aggregate: SelectStats counts
// by_status/by_transport/by_coven/total/stale_count ONLY for hosts within the
// operator's Purview scope. A host outside scope must NOT land in the
// aggregate (otherwise dashboard numbers would diverge from the scoped
// list). Covers stale_count over last_seen_at, the unnest(coven) axis, and
// the fail-closed empty-scope case.
func TestIntegration_SelectStats_Scope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.Add(-10 * time.Minute) // well past the 90s threshold
	fresh := now.Add(-5 * time.Second)

	seed := []struct {
		sid       string
		status    Status
		transport Transport
		coven     []string
		lastSeen  *time.Time
	}{
		// prod-scope hosts:
		{"prod-1.example.com", StatusConnected, TransportAgent, []string{"prod", "dc-eu"}, &fresh},
		{"prod-2.example.com", StatusDisconnected, TransportAgent, []string{"prod"}, &old}, // stale
		{"prod-3.example.com", StatusPending, TransportSSH, []string{"prod"}, nil},         // no last_seen — NOT stale
		// outside prod-scope (must not land in the prod aggregate):
		{"stg-1.example.com", StatusConnected, TransportAgent, []string{"staging"}, &old}, // stale, but outside scope
		{"dev-1.example.com", StatusConnected, TransportSSH, []string{"dev"}, &old},       // stale, but outside scope
	}
	for _, s := range seed {
		row := &Soul{SID: s.sid, Status: s.status, Transport: s.transport, Coven: s.coven, LastSeenAt: s.lastSeen}
		if err := Insert(ctx, integrationPool, row); err != nil {
			t.Fatalf("Insert(%s): %v", s.sid, err)
		}
	}

	stale := 90 * time.Second

	// scope=[prod] → only the 3 prod hosts, no staging/dev in the aggregate.
	prod, err := SelectStats(ctx, integrationPool, ListScope{Covens: []string{"prod"}}, stale)
	if err != nil {
		t.Fatalf("SelectStats(scope=prod): %v", err)
	}
	if prod.Total != 3 {
		t.Errorf("scope=prod Total = %d, want 3 (hosts outside scope are NOT in the aggregate)", prod.Total)
	}
	if prod.ByStatus[StatusConnected] != 1 || prod.ByStatus[StatusDisconnected] != 1 || prod.ByStatus[StatusPending] != 1 {
		t.Errorf("scope=prod ByStatus = %v, want connected:1 disconnected:1 pending:1", prod.ByStatus)
	}
	if prod.ByTransport[TransportAgent] != 2 || prod.ByTransport[TransportSSH] != 1 {
		t.Errorf("scope=prod ByTransport = %v, want agent:2 ssh:1", prod.ByTransport)
	}
	// by_coven: prod=3 (all three), dc-eu=1 (only prod-1). staging/dev are absent.
	if prod.ByCoven["prod"] != 3 || prod.ByCoven["dc-eu"] != 1 {
		t.Errorf("scope=prod ByCoven = %v, want prod:3 dc-eu:1", prod.ByCoven)
	}
	if _, leaked := prod.ByCoven["staging"]; leaked {
		t.Errorf("scope=prod ByCoven leaked staging: %v (scope-leak in the aggregate)", prod.ByCoven)
	}
	if _, leaked := prod.ByCoven["dev"]; leaked {
		t.Errorf("scope=prod ByCoven leaked dev: %v (scope-leak in the aggregate)", prod.ByCoven)
	}
	// stale_count: only prod-2 (prod-3 has no last_seen so is NOT stale; staging/dev are stale but outside scope).
	if prod.StaleCount != 1 {
		t.Errorf("scope=prod StaleCount = %d, want 1 (only prod-2; hosts outside scope are not counted)", prod.StaleCount)
	}

	// unrestricted → the whole fleet: 5 hosts, 3 stale (prod-2, stg-1, dev-1).
	all, err := SelectStats(ctx, integrationPool, ListScope{Unrestricted: true}, stale)
	if err != nil {
		t.Fatalf("SelectStats(unrestricted): %v", err)
	}
	if all.Total != 5 {
		t.Errorf("unrestricted Total = %d, want 5", all.Total)
	}
	if all.StaleCount != 3 {
		t.Errorf("unrestricted StaleCount = %d, want 3 (prod-2, stg-1, dev-1)", all.StaleCount)
	}

	// fail-closed: an empty scope (not Unrestricted) → everything zero, NOT the whole fleet.
	empty, err := SelectStats(ctx, integrationPool, ListScope{Covens: nil}, stale)
	if err != nil {
		t.Fatalf("SelectStats(empty scope): %v", err)
	}
	if empty.Total != 0 || empty.StaleCount != 0 || len(empty.ByStatus) != 0 || len(empty.ByCoven) != 0 {
		t.Errorf("empty scope = %+v, want all zeros (fail-closed, NOT the entire fleet)", empty)
	}
}

// TestIntegration_SelectSoulsWithSoulprint — GUARD (ADR-061 amendment, the
// facts part of the onboarding gate): the batch check returns exactly the
// SIDs with a typed soulprint recorded (`soulprint_facts IS NOT NULL`); a
// freshly-created pending host without a first report is absent; an unknown
// SID is absent.
func TestIntegration_SelectSoulsWithSoulprint(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	for _, sid := range []string{"with-facts.example.com", "factless.example.com"} {
		if err := Insert(ctx, integrationPool, &Soul{SID: sid, Status: StatusPending, Coven: []string{}}); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}
	now := time.Now().UTC()
	if err := UpdateSoulprint(ctx, integrationPool, "with-facts.example.com",
		[]byte(`{"sid":"with-facts.example.com","os":{"family":"debian","arch":"amd64"}}`), now, now); err != nil {
		t.Fatalf("UpdateSoulprint: %v", err)
	}

	got, err := SelectSoulsWithSoulprint(ctx, integrationPool,
		[]string{"with-facts.example.com", "factless.example.com", "unknown.example.com"})
	if err != nil {
		t.Fatalf("SelectSoulsWithSoulprint: %v", err)
	}
	if _, ok := got["with-facts.example.com"]; !ok {
		t.Errorf("with-facts SID missing from result: %v", got)
	}
	if len(got) != 1 {
		t.Errorf("result = %v, want only with-facts SID", got)
	}

	// Empty input — empty result without a query (a nil SID set is legal).
	empty, err := SelectSoulsWithSoulprint(ctx, integrationPool, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("nil sids: got %v, %v; want empty, nil", empty, err)
	}
}
