package scenario

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rowMoverCalls are the writes that move an `apply_runs` row a barrier waits on —
// to a terminal, out of `dispatched` by a withdraw, or the run to Cancel.
var rowMoverCalls = map[string]map[string]bool{
	"applyrun": {
		"UpdateStatus": true, "UpdateWardStatus": true, "FailLost": true, "OrphanDispatched": true,
		"RequestCancel": true, "WithdrawUnsent": true,
	},
	"incarnation": {"RequestRunCancel": true},
}

// rowInsertCalls write a row with whatever status it carries: a mover when the
// unit names a terminal status.
var rowInsertCalls = map[string]bool{"Insert": true, "InsertPlanned": true}

var terminalStatusIdents = map[string]bool{
	"StatusSuccess": true, "StatusNoMatch": true, "StatusFailed": true, "StatusOrphaned": true, "StatusCancelled": true,
}

// wakeExempt names the movers that do not wake, keyed `<path under keeper/> <unit>`,
// with the reason none is needed. A unit is a function, or `<function>.func<N>` for
// its N-th closure: a closure may run in another goroutine than its function.
var wakeExempt = map[string]string{
	"internal/scenario/dispatch.go dispatchWave":               "the run goroutine fails a host whose send failed, then its own barrier reads the row",
	"internal/scenario/keeper_flow.go closeSettledHost":        "the run goroutine closes a host settled on the Keeper, before its own barrier reads it",
	"internal/scenario/keeper_dispatch.go dispatchKeeperTasks": "the run goroutine closes the Keeper's own row inline; no barrier waits on it",
	"internal/scenario/keeper_dispatch.go recordKeeperFailure": "the run goroutine closes the Keeper's own row inline; no barrier waits on it",
	"internal/scenario/run.go ensureTerminalApplyRun":          "the run is ending: its last barrier has returned",
	"internal/grpc/events_wardroster.go handleWardRoster":      "OrphanDispatched returns a count, not the runs; a barrier reads the orphaned row at its fallback poll",
	"internal/scenario/step_watch.go retakeUnsent":             "trySend re-inserts the row it takes back, with the send; a barrier woken in the gap could act on a deferred Cancel while the step still goes out",
}

// silentHelpers are the exempt units another unit calls, each with the unit allowed
// one call of it: any other call is a silent write the scan does not see.
var silentHelpers = map[string]string{
	"retakeUnsent": "internal/scenario/step_watch.go trySend",
}

// ★ GUARD (NIM-984). A barrier sleeps up to defaultBarrierFallback unless the
// write it waits for wakes it, and the writer is often another goroutine or another
// Keeper instance — a Soul's RunResult, a push host, an Acolyte's claim, a lost
// stream, a withdrawn row, a Cancel. A new writer that forgets [applywake.Hub.Wake]
// costs every step of every linear run up to a second, silently: the run still ends
// right.
//
// Blind spots: a write through an aliased import, an Insert whose terminal status
// arrives in a variable or is named outside the closure that inserts, a silent
// helper taken as a method value or its one call moved to another path of its unit,
// and a Wake on another path of the same unit.
//
// Mutation: drop the Wake in terminatePushHost, closeWard, recordUnsent, check,
// withdrawUnsent, RequestCancel, the late-queue closure of dispatchStepPlanned or the
// RunResult handler, withdraw in trySend past retakeUnsent, or call retakeUnsent from
// another withdraw, trySend's own post-send one included → red, naming the unit.
func TestApplyWake_EveryRowMoverWakesTheBarrier(t *testing.T) {
	root := filepath.Join("..", "..")
	movers, wakers := map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "applyrun", "incarnation", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for unit, w := range classifyRowWrites(fn.Name.Name, fn.Body) {
				key := rel + " " + unit
				if w.moves {
					movers[key] = true
					wakers[key] = w.wakes
				}
				for helper, caller := range silentHelpers {
					if n := w.calls[helper]; n > 0 && (key != caller || n > 1) {
						t.Errorf("%s calls %s %d time(s); it writes without waking and is meant for one call in %s", key, helper, n, caller)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	var keys []string
	for key := range movers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch {
		case !wakers[key] && wakeExempt[key] == "":
			t.Errorf("%s moves an apply_runs row and wakes no barrier: call ApplyWake.Wake after the write, or add it to wakeExempt with the reason none waits", key)
		case wakers[key] && wakeExempt[key] != "":
			t.Errorf("%s wakes the barrier and is still in wakeExempt: drop the entry", key)
		}
	}
	for key := range wakeExempt {
		if !movers[key] {
			t.Errorf("wakeExempt names %s, which no longer moves an apply_runs row: drop the entry", key)
		}
	}
	if !movers["internal/grpc/events_runresult.go correlateRunResult"] || !movers["internal/scenario/push_dispatch.go terminatePushHost"] {
		t.Fatalf("the scan found none of the known movers (%d found) — it reads the wrong tree", len(movers))
	}
}

type rowWrites struct {
	moves, wakes bool
	calls        map[string]int
}

// classifyRowWrites reports, for the function body and each closure in it, whether
// it moves a row a barrier waits on and whether it calls a Wake. A closure's calls
// count for the closure only.
func classifyRowWrites(name string, body *ast.BlockStmt) map[string]rowWrites {
	out := map[string]rowWrites{}
	closures := 0
	var walk func(unit string, root ast.Node)
	walk = func(unit string, root ast.Node) {
		w := rowWrites{calls: map[string]int{}}
		inserts, terminal := false, false
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				if n == root {
					return true
				}
				closures++
				walk(fmt.Sprintf("%s.func%d", name, closures), n)
				return false
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				w.calls[sel.Sel.Name]++
				if sel.Sel.Name == "Wake" {
					w.wakes = true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if rowMoverCalls[pkg.Name][sel.Sel.Name] {
					w.moves = true
				}
				if pkg.Name == "applyrun" && rowInsertCalls[sel.Sel.Name] {
					inserts = true
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "applyrun" && terminalStatusIdents[n.Sel.Name] {
					terminal = true
				}
			}
			return true
		})
		w.moves = w.moves || (inserts && terminal)
		out[unit] = w
	}
	walk(name, body)
	return out
}
