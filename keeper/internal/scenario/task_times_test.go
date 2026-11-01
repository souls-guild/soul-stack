package scenario

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	stampSQL  = "INSERT INTO apply_run_plan_host_times"
	finishSQL = "SET finished_at = now()"
	forgetSQL = "DELETE FROM apply_run_plan_host_times"
)

// stampWitness plays the agent stream: it notes how many stamps were written by the
// time it was handed the request.
type stampWitness struct {
	db      *recordingDB
	err     error
	stamped int
}

func (d *stampWitness) SendApply(_ context.Context, _ string, _ *keeperv1.ApplyRequest) error {
	d.stamped = len(d.db.statementsMatching(stampSQL))
	return d.err
}

func timedRequest() *keeperv1.ApplyRequest {
	return &keeperv1.ApplyRequest{ApplyId: "ap-1", Tasks: []*keeperv1.RenderedTask{{PlanIndex: 3}, {PlanIndex: 4}}}
}

// A TaskEvent can come back before SendApply returns, and it closes only a row that
// is already there: the stamp goes out before the request does.
func TestSendTimed_StampsBeforeTheSend(t *testing.T) {
	db := &recordingDB{}
	out := &stampWitness{db: db}
	if err := sendTimed(context.Background(), out, db, discardLog(), "h1.example.com", timedRequest()); err != nil {
		t.Fatalf("sendTimed: %v", err)
	}
	if out.stamped != 1 {
		t.Fatalf("stamps written before the send = %d, want 1", out.stamped)
	}
	args := db.statementsMatching(stampSQL)[0]
	if args[0] != "ap-1" || args[1] != "h1.example.com" || !reflect.DeepEqual(args[2], []int{3, 4}) {
		t.Errorf("stamp args = %v, want (ap-1, h1.example.com, [3 4])", args)
	}
	if n := len(db.statementsMatching(forgetSQL)); n != 0 {
		t.Errorf("a delivered send dropped its stamp (%d deletes)", n)
	}
}

// The run counts the host as not sent: no task of the request may read as started.
func TestSendTimed_FailedSendDropsTheStamp(t *testing.T) {
	db := &recordingDB{}
	out := &stampWitness{db: db, err: errors.New("no stream")}
	if err := sendTimed(context.Background(), out, db, discardLog(), "h1.example.com", timedRequest()); err == nil {
		t.Fatal("sendTimed swallowed the send error")
	}
	forgets := db.statementsMatching(forgetSQL)
	if len(forgets) != 1 || !reflect.DeepEqual(forgets[0][2], []int{3, 4}) {
		t.Errorf("deletes = %v, want one over [3 4]", forgets)
	}
}

// A push host's step is stamped before the SSH session runs it, and its TaskEvents
// close their rows through the same sink a streamed host's do.
func TestDispatchPushHost_StampsTheStepAndItsEvents(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{{ApplyId: "ap-1", PlanIndex: 3, Status: keeperv1.TaskStatus_TASK_STATUS_OK}},
		result: &keeperv1.RunResult{ApplyId: "ap-1", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-1"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, timedRequest())

	stamp, finish := -1, -1
	for i, s := range db.sqls {
		switch {
		case strings.Contains(s, stampSQL) && stamp < 0:
			stamp = i
		case strings.Contains(s, finishSQL) && finish < 0:
			finish = i
		}
	}
	if stamp < 0 || finish < 0 || stamp > finish {
		t.Fatalf("stamp at %d, finish at %d: want the stamp written, then the event's finish", stamp, finish)
	}
	if args := db.args[finish]; args[0] != "ap-1" || args[1] != "h1.example.com" || args[2] != 3 || args[3] != true {
		t.Errorf("finish args = %v, want (ap-1, h1.example.com, 3, start the next)", args)
	}
}

// A connect that failed started nothing on the host.
func TestDispatchPushHost_FailedConnectDropsTheStamp(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{err: fmt.Errorf("%w: dial tcp: refused", push.ErrConnect)}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-1"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, timedRequest())

	if len(db.statementsMatching(stampSQL)) != 1 || len(db.statementsMatching(forgetSQL)) != 1 {
		t.Errorf("stamps = %d, deletes = %d; want the stamp written and dropped",
			len(db.statementsMatching(stampSQL)), len(db.statementsMatching(forgetSQL)))
	}
}

// timedSenders are the only functions of the package that hand a run's tasks to a
// host, each stamping the tasks first.
var timedSenders = map[string]bool{"sendTimed": true, "pushOnce": true}

// ★ GUARD (NIM-1170). A Soul task's time rows are written by its send: the first
// task starts there, and the task's TaskEvent closes only a row that is already
// there. A send that skips [sendTimed] or [Runner.pushOnce] leaves every task of
// that host without a time, silently: the run still ends right.
//
// Blind spot: a send through a method value or a variable holding the dispatcher's
// method.
//
// Mutation: call Outbound.SendApply directly in dispatchWave, trySend or the claim's
// dispatch, PushApply.SendApply directly in sendPush, or move stampSent after the
// send in sendTimed or pushOnce → red, naming the function.
func TestTaskTimes_EverySendStampsFirst(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			stamp, send := token.NoPos, token.NoPos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callee := call.Fun.(type) {
				case *ast.SelectorExpr:
					if callee.Sel.Name == "SendApply" && send == token.NoPos {
						send = call.Pos()
					}
				case *ast.Ident:
					if callee.Name == "stampSent" && stamp == token.NoPos {
						stamp = call.Pos()
					}
				}
				return true
			})
			if send == token.NoPos {
				continue
			}
			name := fn.Name.Name
			if !timedSenders[name] {
				t.Errorf("%s: %s hands a host its tasks past sendTimed/pushOnce: their times are never written", fset.Position(send), name)
				continue
			}
			seen[name] = true
			if stamp == token.NoPos || stamp > send {
				t.Errorf("%s: %s sends before it stamps: a fast TaskEvent finds no row to close", fset.Position(send), name)
			}
		}
	}
	for name := range timedSenders {
		if !seen[name] {
			t.Errorf("%s sends nothing any more: the guard's list is stale", name)
		}
	}
}
