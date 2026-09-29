package render

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

// Per-host params on the wire (NIM-908, closing open Q #25 for params).
//
// The wire was always per-host — each SID gets its own ApplyRequest — but the
// converter had to be TOLD which host it was building for, and two call sites were
// not telling it. These tests pin the two halves: the converter answers with this
// host's struct, and no dispatch path can quietly go back to asking without a SID.

// toProtoTasksCallers — every call to the sid-less [ToProtoTasks] in the keeper
// module, with the reason it is allowed to flatten a plan onto one params struct.
// The table IS the guard: the AST walk below fails on a call that is not listed, so
// a new dispatch path has to answer "can this be multi-host?" before it ships.
//
// The hazard is silent by construction. ToProtoTasks returns no error and always
// answers — with [RenderedTask.Params], the FIRST host by SID — so a multi-host
// caller that forgets its SID reproduces exactly the defect NIM-908 removed, and
// every test still passes.
var toProtoTasksCallers = map[string]string{
	"keeper/internal/trial/l2_run.go": "L2 applies one ApplyRequest to one stand; a plan with per-host params is REFUSED " +
		"just above the call (render.PlanIsPerHost), so nothing is flattened",
}

// TestProtoTasks_EveryMultiHostDispatchPathPassesASID walks the whole repository's
// sources for calls to ToProtoTasks and checks each against the table.
//
// KNOWN LIMIT, stated rather than implied: it matches a direct call by name, so
// `f := render.ToProtoTasks; f(tasks)` evades it. That shape appears nowhere in the
// tree and would be odd to write; the guard is against forgetting a SID on a new
// dispatch path, not against deliberate indirection.
//
// NON-VACUITY: change pushorch's fanOut back to converting once, outside the
// per-SID loop, and this goes red naming the file — which is how that call site was
// found in the first place.
func TestProtoTasks_EveryMultiHostDispatchPathPassesASID(t *testing.T) {
	// The whole REPOSITORY, not just the keeper module: a dispatch path added in
	// soulctl/ or tests/ would be invisible to a module-scoped walk, and the hazard
	// is not module-local.
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	walked := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not ours to report; the compiler says it better
		}
		walked++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isToProtoTasksCall(call.Fun) {
				return true
			}
			// The declaration and its own wrapper live here and are not callers.
			if rel == "keeper/internal/render/prototask.go" {
				return true
			}
			seen[rel] = true
			if _, allowed := toProtoTasksCallers[rel]; !allowed {
				t.Errorf("%s: calls ToProtoTasks (no SID), which answers with the FIRST host by SID "+
					"for every host. If this path can be multi-host, use ToProtoTasksForHost(tasks, sid); "+
					"if it cannot, add it to toProtoTasksCallers with the reason",
					fset.Position(call.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if walked == 0 {
		t.Fatal("parsed no sources at all — the walk is broken, and a broken walk gates nothing")
	}
	for path := range toProtoTasksCallers {
		if !seen[path] {
			t.Errorf("toProtoTasksCallers lists %q, but it no longer calls ToProtoTasks — "+
				"stale entry; a stale allowlist hides the next one", path)
		}
	}
}

func isToProtoTasksCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "ToProtoTasks"
	case *ast.SelectorExpr:
		return f.Sel.Name == "ToProtoTasks"
	}
	return false
}

// paramsBySIDTask builds a RenderedTask carrying two hosts' params.
func paramsBySIDTask(t *testing.T) *RenderedTask {
	t.Helper()
	mk := func(cmd string) *structpb.Struct {
		st, err := structpb.NewStruct(map[string]any{"cmd": cmd, "shared": "same"})
		if err != nil {
			t.Fatalf("structpb: %v", err)
		}
		return st
	}
	return &RenderedTask{
		Index:  0,
		Name:   "echo",
		Module: "core.exec.run",
		Params: mk("echo a"),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": mk("echo a"),
			"b.example.com": mk("echo b"),
		},
	}
}

// The converter answers with THIS host's struct, and the sid-less wrapper still
// answers with the golden-path one — the precondition its doc states.
func TestProtoTasks_ParamsBySIDIsSelectedPerHost(t *testing.T) {
	tasks := []*RenderedTask{paramsBySIDTask(t)}

	for sid, want := range map[string]string{"a.example.com": "echo a", "b.example.com": "echo b"} {
		got := ToProtoTasksForHost(tasks, sid)[0].Params.GetFields()["cmd"].GetStringValue()
		if got != want {
			t.Errorf("%s: cmd = %q, want %q", sid, got, want)
		}
	}
	if got := ToProtoTasks(tasks)[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo a" {
		t.Errorf("sid-less conversion: cmd = %q, want the golden-path %q", got, "echo a")
	}
	// A SID the map does not carry falls back to the golden path rather than to an
	// empty struct — a host outside this task's target never receives the request at
	// all, and a nil params would be a panic on the way to nobody.
	if got := ToProtoTasksForHost(tasks, "c.example.com")[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo a" {
		t.Errorf("unknown SID: cmd = %q, want the golden-path %q", got, "echo a")
	}
	// And the plan's shared struct is not mutated by any of that.
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo a" {
		t.Errorf("the shared Params struct was mutated: %q", got)
	}
}

func TestProtoTasks_PlanIsPerHost(t *testing.T) {
	if PlanIsPerHost([]*RenderedTask{{Name: "plain"}}) {
		t.Error("a plan with no per-host params reported as per-host")
	}
	if !PlanIsPerHost([]*RenderedTask{{Name: "plain"}, paramsBySIDTask(t)}) {
		t.Error("a plan with one per-host task reported as host-invariant")
	}
}

// The operator-facing plan row carries ONE params map beside per-host results. A
// cell that differs is MARKED; the cells that do not are still shown, because
// blanking the whole map to protect one key would cost the view its point.
func TestPlanParamsForDisplay_MarksOnlyTheCellsThatVary(t *testing.T) {
	got := PlanParamsForDisplay(paramsBySIDTask(t))
	if got["cmd"] != PerHostParamValue {
		t.Errorf("params.cmd = %v, want %q — the view shows the first host's value as everyone's", got["cmd"], PerHostParamValue)
	}
	if got["shared"] != "same" {
		t.Errorf("params.shared = %v, want %q — a host-invariant cell must stay visible", got["shared"], "same")
	}

	// Golden path: no per-host map → the params map, unchanged.
	plain := paramsBySIDTask(t)
	plain.ParamsBySID = nil
	if got := PlanParamsForDisplay(plain); got["cmd"] != "echo a" {
		t.Errorf("golden path altered: params.cmd = %v, want %q", got["cmd"], "echo a")
	}
}
