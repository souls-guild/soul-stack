package conductor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
)

// Coven-scoped console grants on the background choke-point (NIM-650).
//
// The identity judged here is the recipe's AUTHOR (`created_by_aid`) — an
// operator like any other, so `soul.console on coven=web` is a shape this path
// must expect. With the host-only context the gate used to build, such an author's
// schedule would have started recording `console_required` skips forever the day
// enforcement turned on: no HTTP status, no operator watching, only an audit
// event nobody is reading yet.
//
// Every case runs under BOTH ways a scope reaches the resolver — the permission's
// `on coven=web` suffix and the role's `default_scope`. They are different columns
// and unify only inside rbac.effectiveScope; a guard on one says nothing about
// the other.
//
// A real *rbac.Enforcer, not the gateChecker fake above: the point of these tests
// is the scope comparison itself, and a fake that answers by host would agree with
// itself no matter what the enforcer does.

const (
	covenWebHost  = "web-01.example.com"
	covenProdHost = "db-01.example.com"
	covenBothHost = "edge-01.example.com"
)

// covenReader — the souls-table read the gate makes, batched. `Query` is the
// only method the path reaches; queries counts round-trips, so a test can assert
// the whole target costs exactly one.
type covenReader struct {
	byHost  map[string][]string
	queries int
	fail    bool
}

func (r *covenReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), nil
}

func (r *covenReader) QueryRow(context.Context, string, ...any) pgx.Row {
	return covenErrRow{}
}

func (r *covenReader) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	r.queries++
	if r.fail {
		return nil, pgx.ErrTxClosed
	}
	sids, _ := args[0].([]string)
	rows := &covenRows{}
	for _, sid := range sids {
		if labels, ok := r.byHost[sid]; ok {
			rows.sids = append(rows.sids, sid)
			rows.labels = append(rows.labels, labels)
		}
	}
	return rows, nil
}

type covenErrRow struct{}

func (covenErrRow) Scan(...any) error { return pgx.ErrNoRows }

type covenRows struct {
	sids   []string
	labels [][]string
	i      int
}

func (r *covenRows) Next() bool {
	r.i++
	return r.i <= len(r.sids)
}

func (r *covenRows) Scan(dest ...any) error {
	if len(dest) >= 2 {
		if out, ok := dest[0].(*string); ok {
			*out = r.sids[r.i-1]
		}
		if out, ok := dest[1].(*[]string); ok {
			*out = r.labels[r.i-1]
		}
	}
	return nil
}

func (r *covenRows) Close()                                       {}
func (r *covenRows) Err() error                                   { return nil }
func (r *covenRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("") }
func (r *covenRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *covenRows) Values() ([]any, error)                       { return nil, nil }
func (r *covenRows) RawValues() [][]byte                          { return nil }
func (r *covenRows) Conn() *pgx.Conn                              { return nil }

func newCovenReader() *covenReader {
	return &covenReader{byHost: map[string][]string{
		covenWebHost:  {"web"},
		covenProdHost: {"prod"},
		covenBothHost: {"prod", "web"},
	}}
}

// spawnCovenForm — the two routes a coven scope takes to the resolver.
type spawnCovenForm struct {
	name string
	enf  func(t *testing.T) *rbac.Enforcer
}

func spawnCovenForms() []spawnCovenForm {
	return []spawnCovenForm{
		{
			name: "scope on the permission",
			enf: func(t *testing.T) *rbac.Enforcer {
				t.Helper()
				return rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"},
						Permissions: []string{"soul.console on coven=web"}},
				}})
			},
		},
		{
			name: "default_scope on the role",
			enf: func(t *testing.T) *rbac.Enforcer {
				t.Helper()
				return rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"},
						Permissions: []string{"soul.console"}, DefaultScope: "coven=web"},
				}})
			},
		},
	}
}

// covenSpawner wires an enforcing gate, a real enforcer and the coven reader.
func covenSpawner(tx *spawnFakeTx, hosts []string, enf ConsoleChecker, reader *covenReader) *CadenceSpawner {
	s := newSpawnerFor(tx, stubResolver{out: hosts}, nil)
	s.enforcer = enf
	s.gate = shellgate.New(shellgate.ModeEnforce, nil, nil)
	if reader != nil {
		s.soulReader = reader
	}
	return s
}

// spawnVerdict runs one due verb-shell Cadence and reports whether it spawned.
// A refusal must never be an error — that would roll back the tick and stall
// every other due schedule — so an error here is a failure of the test's premise,
// not one of its outcomes.
func spawnVerdict(t *testing.T, s *CadenceSpawner, tx *spawnFakeTx, module string) bool {
	t.Helper()
	_, spawned, err := s.processOne(context.Background(), tx, commandCadence(module), time.Now())
	if err != nil {
		t.Fatalf("processOne returned an error; a refusal must be a recorded skip: %v", err)
	}
	return spawned
}

// TestSpawn_CovenScopedAuthorSpawnsInHerCoven — the pair. Narrowing that admitted
// every host would satisfy the spawn half alone, and the pre-fix
// refuse-everything behaviour would satisfy the skip half alone.
func TestSpawn_CovenScopedAuthorSpawnsInHerCoven(t *testing.T) {
	for _, form := range spawnCovenForms() {
		t.Run(form.name, func(t *testing.T) {
			tx := &spawnFakeTx{}
			s := covenSpawner(tx, []string{covenWebHost}, form.enf(t), newCovenReader())
			if !spawnVerdict(t, s, tx, "core.cmd.shell") {
				t.Error("a target inside the author's coven did not spawn — the background gate " +
					"still reads a coven-scoped `soul.console` as a denial, and the schedule " +
					"stops producing runs with nobody watching")
			}

			tx = &spawnFakeTx{}
			s = covenSpawner(tx, []string{covenProdHost}, form.enf(t), newCovenReader())
			if spawnVerdict(t, s, tx, "core.cmd.shell") {
				t.Error("`soul.console on coven=web` spawned a shell recipe against a host in coven prod")
			}
			if tx.execCalls != 1 {
				t.Errorf("the schedule must still advance on a skip; exec=%d, want 1", tx.execCalls)
			}
		})
	}
}

// TestSpawn_CovenScopeIsAllOrNothing — the probe requires the right on EVERY
// resolved host. A partial spawn would silently rewrite the recipe the operator
// wrote into one that runs on a subset, and nothing downstream would say so.
func TestSpawn_CovenScopeIsAllOrNothing(t *testing.T) {
	for _, form := range spawnCovenForms() {
		t.Run(form.name, func(t *testing.T) {
			tx := &spawnFakeTx{}
			s := covenSpawner(tx, []string{covenWebHost, covenProdHost}, form.enf(t), newCovenReader())
			if spawnVerdict(t, s, tx, "core.cmd.shell") {
				t.Error("a target half inside the grant spawned — the recipe now runs on a subset " +
					"of the hosts it names")
			}
			if tx.insertCalls != 0 {
				t.Errorf("a refused spawn inserted a voyage; insert=%d", tx.insertCalls)
			}
		})
	}
}

// TestSpawn_MultiCovenHostIsAdmittedByAnyOfItsCovens — ADR-008: a host carries a
// LIST of labels and an rbac.Permission holds one value per dimension, so the
// check is an OR over one context per label. Collapsing the list to its first
// element would refuse edge-01 (first label: prod) while every single-coven case
// above stayed green.
func TestSpawn_MultiCovenHostIsAdmittedByAnyOfItsCovens(t *testing.T) {
	for _, form := range spawnCovenForms() {
		t.Run(form.name, func(t *testing.T) {
			tx := &spawnFakeTx{}
			s := covenSpawner(tx, []string{covenBothHost}, form.enf(t), newCovenReader())
			if !spawnVerdict(t, s, tx, "core.cmd.shell") {
				t.Error("a host in {prod, web} was refused by a grant on coven=web — only one of " +
					"its labels reached the check")
			}
		})
	}
}

// TestSpawn_UnreadableCovenFailsClosed — a nil reader, a host with no row and a
// failed read all produce the `{host}` context alone. Coven-scoped grants must
// then refuse, and host-scoped ones must keep working: the fallback is the
// pre-NIM-650 behaviour, confined to the case where the coven genuinely is
// unknown.
func TestSpawn_UnreadableCovenFailsClosed(t *testing.T) {
	webOnly := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "web-ops", Operators: []string{"archon-alice"},
			Permissions: []string{"soul.console on coven=web"}},
	}})
	hostOnly := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "one-host", Operators: []string{"archon-alice"},
			Permissions: []string{"soul.console on host=" + covenWebHost}},
	}})

	cases := []struct {
		name   string
		reader *covenReader
	}{
		{"no reader wired", nil},
		{"the read fails", &covenReader{fail: true}},
		{"the host has no row", &covenReader{byHost: map[string][]string{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &spawnFakeTx{}
			if spawnVerdict(t, covenSpawner(tx, []string{covenWebHost}, webOnly, tc.reader),
				tx, "core.cmd.shell") {
				t.Error("a coven-scoped grant spawned against a host whose coven could not be read")
			}

			tx = &spawnFakeTx{}
			if !spawnVerdict(t, covenSpawner(tx, []string{covenWebHost}, hostOnly, tc.reader),
				tx, "core.cmd.shell") {
				t.Error("a plain host-scoped grant stopped spawning — the coven work broke the " +
					"case it was supposed to leave alone")
			}
		})
	}
}

// TestSpawn_CovenReadIsOneRoundTripForTheWholeTarget — a Cadence target resolves
// to a snapshot that may run to hundreds of hosts, and this runs inside the tick's
// transaction between the FOR UPDATE select and AdvanceSchedule. A read per host
// would put that many round-trips in the way of every other due schedule.
func TestSpawn_CovenReadIsOneRoundTripForTheWholeTarget(t *testing.T) {
	reader := newCovenReader()
	tx := &spawnFakeTx{}
	enf := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "web-ops", Operators: []string{"archon-alice"},
			Permissions: []string{"soul.console on coven=web"}},
	}})
	s := covenSpawner(tx, []string{covenWebHost, covenBothHost}, enf, reader)

	if !spawnVerdict(t, s, tx, "core.cmd.shell") {
		t.Fatal("both hosts are in coven web; the spawn should have gone through")
	}
	if reader.queries != 1 {
		t.Errorf("coven reads = %d, want 1 for the whole target", reader.queries)
	}
}

// TestSpawn_OrdinaryModuleNeverReadsCovens — shellgate.Gate.Authorize does not
// call the probe for a module that is not a verb shell, so the ordinary Cadence
// (the overwhelming majority) pays nothing for this. The reader records every
// touch; the assertion is that there are none.
func TestSpawn_OrdinaryModuleNeverReadsCovens(t *testing.T) {
	reader := newCovenReader()
	tx := &spawnFakeTx{}
	enf := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "runner", Operators: []string{"archon-alice"}, Permissions: []string{"errand.run"}},
	}})
	s := covenSpawner(tx, []string{covenProdHost}, enf, reader)

	if !spawnVerdict(t, s, tx, "core.http.probe") {
		t.Fatal("a read-safe module must spawn without soul.console")
	}
	if reader.queries != 0 {
		t.Errorf("coven reads = %d, want 0 — an ordinary Cadence now pays a souls round-trip "+
			"inside the tick transaction", reader.queries)
	}
}
