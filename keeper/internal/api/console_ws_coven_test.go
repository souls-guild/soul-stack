package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	soulpkg "github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// NIM-650: the LIVE console. The per-`open` scope check ran with a `{host}`
// context alone, so `soul.console on coven=web` refused every host — including
// the ones in coven web — while the recording playback of those same sessions
// narrowed correctly through ResolvePurview. The socket is the surface the
// right is named for; it was the one place the label did not work.

// wsCovenReader is the souls read surface the `open` handler resolves the
// target's Coven labels through. Only QueryRow is reachable from here — one
// SID per frame.
type wsCovenReader map[string][]string

func (r wsCovenReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("wsCovenReader: Exec not expected")
}
func (r wsCovenReader) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("wsCovenReader: Query not expected")
}
func (r wsCovenReader) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	sid, _ := args[0].(string)
	covens, ok := r[sid]
	if !ok {
		return wsCovenScanRow{err: pgx.ErrNoRows}
	}
	return wsCovenScanRow{covens: covens}
}

type wsCovenScanRow struct {
	covens []string
	err    error
}

func (r wsCovenScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*[]string)) = r.covens
	return nil
}

// covenScopedConsoleRBAC builds a real enforcer granting `soul.console` narrowed
// to coven web, in whichever of the two forms the caller asks for.
func covenScopedConsoleRBAC(t *testing.T, onPermission bool) *rbac.Enforcer {
	t.Helper()
	role := rbactest.Role{Name: "web-ops", Operators: []string{consoleTestAID}}
	if onPermission {
		role.Permissions = []string{"soul.console on coven=web"}
	} else {
		role.Permissions = []string{"soul.console"}
		role.DefaultScope = "coven=web"
	}
	return rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{role}})
}

func TestConsoleWS_CovenScopedRoleOpensAHostInItsCoven(t *testing.T) {
	reader := wsCovenReader{
		"web-01.example.com": {"web"},
		"db-99.example.com":  {"db"},
	}
	for _, form := range []struct {
		name         string
		onPermission bool
	}{
		{"scope on the permission", true},
		{"default_scope on the role", false},
	} {
		t.Run(form.name, func(t *testing.T) {
			enf := covenScopedConsoleRBAC(t, form.onPermission)
			s := newConsoleTestServer(t, enf, console.Limits{},
				func(d *consoleWSDeps) { d.SoulReader = reader })
			ws := s.dial(t)

			// The host IS in coven web: the label must narrow, not refuse.
			writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p1", "sid": "web-01.example.com"})
			readFrameOfType(t, ws, "opened")
			if s.hub.Count() != 1 {
				t.Fatalf("live sessions = %d, want 1 — a `coven=web` grant must open a host in coven web",
					s.hub.Count())
			}

			// And it still bites outside the coven.
			writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p2", "sid": "db-99.example.com"})
			errFrame := readFrameOfType(t, ws, "error")
			if errFrame["code"] != console.ErrCodeForbidden {
				t.Fatalf("error.code = %v, want %s for a host outside the coven",
					errFrame["code"], console.ErrCodeForbidden)
			}
		})
	}
}

func TestConsoleWS_CovenScopedRoleIsRefusedOnAnUnknownSID(t *testing.T) {
	// A SID with no souls row. The coven is then genuinely unknown, and the
	// grant must fail closed — otherwise naming a host that does not exist
	// would be a way past a label an operator does not hold.
	enf := covenScopedConsoleRBAC(t, true)
	s := newConsoleTestServer(t, enf, console.Limits{},
		func(d *consoleWSDeps) { d.SoulReader = wsCovenReader{"web-01.example.com": {"web"}} })
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p1", "sid": "ghost.example.com"})
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeForbidden)
	}
	if s.hub.Count() != 0 {
		t.Fatal("a session was opened for a host whose coven could not be read")
	}
}

func TestConsoleWS_MultiCovenHostIsAdmittedByAnyOfItsCovens(t *testing.T) {
	enf := covenScopedConsoleRBAC(t, true)
	s := newConsoleTestServer(t, enf, console.Limits{},
		func(d *consoleWSDeps) {
			d.SoulReader = wsCovenReader{"multi.example.com": {"db", "eu", "web"}}
		})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p1", "sid": "multi.example.com"})
	readFrameOfType(t, ws, "opened")
	if s.hub.Count() != 1 {
		t.Fatalf("live sessions = %d, want 1 — a host carries a LIST of covens (ADR-008) and a "+
			"grant on any one of them covers it", s.hub.Count())
	}
}

func TestConsoleWS_NilSoulReaderKeepsHostScopedRolesWorking(t *testing.T) {
	// The wiring is nil-safe, and the fallback is the pre-NIM-650 context set:
	// `host=` still resolves, `coven=` fails closed.
	enf := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "one-host", Operators: []string{consoleTestAID},
			Permissions: []string{"soul.console on host=web-01.example.com"}},
	}})
	s := newConsoleTestServer(t, enf, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p1", "sid": "web-01.example.com"})
	readFrameOfType(t, ws, "opened")
}

// TestConsoleWS_ContextsMatchTheRESTSelector pins that the socket and the routes
// build the SAME context set. They are different call sites — the socket reads
// the SID off a frame, the routes off the path — and the bug NIM-650 fixes was
// exactly the two drifting apart.
func TestConsoleWS_ContextsMatchTheRESTSelector(t *testing.T) {
	t.Parallel()
	reader := wsCovenReader{"web-01.example.com": {"web", "eu"}}
	got := soulpkg.HostContextsBySID(context.Background(), reader, "web-01.example.com")
	want := soulpkg.HostContexts("web-01.example.com", []string{"web", "eu"})
	if len(got) != len(want) {
		t.Fatalf("contexts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i]["host"] != want[i]["host"] || got[i]["coven"] != want[i]["coven"] {
			t.Fatalf("contexts[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
