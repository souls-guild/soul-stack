package console

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"
	"time"
)

// The envelope is re-read on every check, not frozen at construction.
//
// This is the live apply path ADR-0073(j.5) demands before a key may be served
// from the SettingsStore overlay: without it the UI would accept an edit to the
// ceilings and change nothing until the next restart — silently, which is the
// failure `requires_restart` exists to make loud. A static Hub would pass every
// other test in this package, so the moving value is the only thing that pins it.
func TestHub_CeilingsAreReResolvedPerOpen(t *testing.T) {
	var ceiling atomic.Int64
	ceiling.Store(1)
	h, _ := newTestHub(t, HubDeps{Limits: func() Limits {
		return Limits{MaxSessionsPerAID: int(ceiling.Load())}
	}})
	sink := &captureSink{}

	mustOpen(t, h, "a", "host-a", "archon-a", sink)
	if _, err := h.Open(context.Background(), OpenRequest{
		ClientID: "b", SID: "host-b", AID: "archon-a", Sink: sink,
	}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("second open under a ceiling of 1: err = %v, want ErrLimitExceeded", err)
	}

	// The operator raises the ceiling. No restart, no new Hub.
	ceiling.Store(2)
	mustOpen(t, h, "b", "host-b", "archon-a", sink)

	// And lowering it again bites the next open rather than the live sessions.
	ceiling.Store(1)
	if _, err := h.Open(context.Background(), OpenRequest{
		ClientID: "c", SID: "host-c", AID: "archon-a", Sink: sink,
	}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("open after lowering the ceiling: err = %v, want ErrLimitExceeded", err)
	}
	if h.Count() != 2 {
		t.Fatalf("live sessions = %d, want 2 — lowering a ceiling must not retroactively close sessions", h.Count())
	}
}

// Same contract for the idle sweep, whose timeout is the third overlay key.
func TestHub_IdleTimeoutIsReResolvedPerSweep(t *testing.T) {
	var timeout atomic.Int64
	timeout.Store(int64(time.Hour))
	h, _ := newTestHub(t, HubDeps{Limits: func() Limits {
		return Limits{IdleTimeout: time.Duration(timeout.Load())}
	}})
	mustOpen(t, h, "a", "host-a", "archon-a", &captureSink{})

	if n := h.SweepIdle(context.Background()); n != 0 {
		t.Fatalf("swept %d sessions under an hour-long timeout, want 0", n)
	}

	// Tightened to something every live session already exceeds.
	timeout.Store(int64(time.Nanosecond))
	time.Sleep(time.Millisecond)
	if n := h.SweepIdle(context.Background()); n != 1 {
		t.Fatalf("swept %d sessions after tightening the timeout, want 1", n)
	}
}

// A nil provider is the defaults, so a zero HubDeps stays usable — the shape
// every unit test in this package relies on.
func TestHub_NilLimitsProviderResolvesToDefaults(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	got := h.currentLimits()
	if got.MaxSessionsPerAID != DefaultMaxSessionsPerAID ||
		got.MaxSessionsGlobal != DefaultMaxSessionsGlobal ||
		got.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("currentLimits() = %+v, want the package defaults", got)
	}
}

// The frame plane has exactly one home, and it is not this package.
//
// Queue depth, socket deadlines and the inbound frame cap describe the WebSocket
// implementation in internal/api, which owns them. This package once carried a
// second copy of all five, referenced by nothing: Go does not complain about an
// unused constant, so editing the dead copy changed nothing, while editing the
// live one drifted away from a comment claiming the two were mirrors. Both
// failures are silent, which is why this guards the split rather than the values
// — a re-added copy is the defect whatever it is set to.
func TestFramePlaneConstantsHaveNoSecondHomeHere(t *testing.T) {
	owned := map[string]string{
		"outQueueDepth":      "consoleOutQueueDepth",
		"writeWait":          "consoleWriteWait",
		"pongWait":           "consolePongWait",
		"pingPeriod":         "consolePingPeriod",
		"maxClientFrameSize": "consoleMaxClientFrame",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if live, dup := owned[name.Name]; dup {
							t.Errorf("%s declares %q — the frame plane belongs to internal/api (%s). A copy here is referenced by nothing, so it drifts in silence: Go does not flag an unused constant.",
								path, name.Name, live)
						}
					}
				}
			}
		}
	}
}
