//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

// Guards for the harness invariants that are CALLS rather than values.
//
// NIM-548. NIM-469 fixed six mechanisms in this package and guarded the shape of
// each one; a mutation run afterwards walked through six of those guards, all in
// the same way. Go does not complain about a method nobody calls, so deleting
// `stdoutLog.stop()`, or swapping an `append` for a `Close()`, or moving one line
// above another, compiles silently — and a test that proves the mechanism WORKS
// says nothing about whether anything still invokes it. TestLogWriter_Stop
// passed the whole time the call was gone.
//
// So these ask a different question from the tests next to them: not "is the
// mechanism correct" but "is it still wired in, in the position that makes it
// work". Where the property can be observed at runtime without docker it is
// observed (reserveLoopback below actually binds a port); where it is about the
// arrangement of statements it is read out of the AST.
//
// The recurring mistake these are written against is naming the subject instead
// of deriving it — see waitstrategy_test.go's containerRaisers for the same
// correction. Each guard here starts from a property of the code ("assigns a
// testLogWriter", "renders a keeper config", "terminates a container") and every
// one carries a count backstop, because the way this class of guard dies is by
// finding nothing and reporting success.

// TestReserveLoopbackHoldsTheAddress — the reservation is a HELD listener, not a
// remembered number.
//
// This is NIM-469's central bug, and a mutation restored it by replacing the
// append with a Close() — two tokens, no test anywhere went red. The old code
// closed the socket at allocation and wrote the address into keeper.yml, leaving
// the port free for the whole of `keeper init`: seconds, on the loopback
// ephemeral range every other client on the box also draws from. Losing that
// race either kills the test on a /readyz deadline that never mentions a port,
// or hands it a foreign process that answers /readyz with 200 (probe.go).
//
// Observed rather than inspected. The claim is "nothing else can bind this
// address until we let go", so the test tries to bind it, and then tries again
// after releasing. No docker, no keeper — this runs in `make check-e2e-set`.
func TestReserveLoopbackHoldsTheAddress(t *testing.T) {
	s := &Stack{t: t}

	addr := s.reserveLoopback()
	if len(s.portReservations) != 1 {
		t.Fatalf("reserveLoopback recorded %d reservations, want 1. The listener is what "+
			"holds the address; an address returned without one is just a number that was "+
			"free a moment ago.", len(s.portReservations))
	}

	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("%s could be bound by someone else while the stack held it reserved. That "+
			"is the whole of the reservation: between rendering keeper.yml and the keeper's "+
			"bind, this port must not be available to the pgx pool, the Vault client, "+
			"testcontainers or a neighbouring suite.", addr)
	}

	second := s.reserveLoopback()
	if second == addr {
		t.Fatalf("two reservations returned the same address %q — the kernel cannot have "+
			"handed out a bound port twice, so the first one is not being held.", addr)
	}

	s.releasePortReservations()
	if len(s.portReservations) != 0 {
		t.Fatalf("releasePortReservations left %d reservations behind; a stack that never "+
			"reaches the keeper would hold them for the rest of the run.", len(s.portReservations))
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s is still held after releasePortReservations: %v. The keeper binds these "+
			"addresses itself, so a reservation that outlives the release stops the process "+
			"it was protecting.", addr, err)
	}
	_ = l.Close()
}

// TestReservationsAreReleasedAtTheBind — the release happens in the last instant
// before the process binds, and nowhere else.
//
// The window between releasing and binding is the whole risk, and its size is a
// matter of statement ORDER, which no runtime test in this package can see. A
// mutation moved the call up into NewStack, ahead of `keeper init` — restoring
// the original seconds-long window exactly, with every other guard green.
//
// Two properties, because either one alone is satisfiable by the mutation. The
// call must live in a function that also starts the keeper process, and inside
// that function it must be the statement immediately before the start. The
// teardown path (runCleanups) is the one sanctioned exception: it releases the
// listeners of stacks that never got as far as a keeper.
func TestReservationsAreReleasedAtTheBind(t *testing.T) {
	const release = "releasePortReservations"
	sites := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		if base == "config_builder.go" {
			return // the declaration itself
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if !callsMethod(fn.Body, release) {
				continue
			}
			sites++
			if fn.Name.Name == "runCleanups" {
				continue
			}
			if !callsMethod(fn.Body, "Start") {
				t.Errorf("%s: %s calls %s() but never starts a process. The reservation is "+
					"released for a bind that is not in this function, so the address sits "+
					"free for however long the rest of the bring-up takes — which is the "+
					"seconds-long window NIM-469 was about.",
					fset.Position(fn.Pos()), fn.Name.Name, release)
				continue
			}
			if !releasedImmediatelyBeforeStart(fn.Body, release) {
				t.Errorf("%s: in %s, %s() is not the statement immediately before the "+
					"process start. Every statement between the two is time in which "+
					"anything on the host can take the port the keeper is about to bind.",
					fset.Position(fn.Pos()), fn.Name.Name, release)
			}
		}
	})
	if sites == 0 {
		t.Fatalf("nothing outside config_builder.go calls %s(). Either the reservations are "+
			"never handed back — in which case the keeper cannot bind at all — or the method "+
			"was renamed and this guard is checking an empty set.", release)
	}
}

// releasedImmediatelyBeforeStart looks for the two calls as ADJACENT statements
// of one block, which is the property: not "both appear somewhere".
func releasedImmediatelyBeforeStart(body *ast.BlockStmt, release string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i := 1; i < len(block.List); i++ {
			if callsMethod(block.List[i], "Start") && callsMethod(block.List[i-1], release) {
				found = true
			}
		}
		return true
	})
	return found
}

// TestKeeperLogWritersAreDetached — every log writer handed to a subprocess is
// detached from the test before the test ends.
//
// testLogWriter forwards the keeper's stdout to t.Logf, and a *testing.T that
// gets logged to after its test has finished panics the run — in a test that did
// nothing wrong, since the writer belongs to whichever stack spawned the keeper
// that is still draining its pipe. NIM-469 added stop() and calls it in the
// cleanup; a mutation deleted the two calls and nothing noticed, because the
// unit test for stop() tests the method, not its use.
//
// Derived from "a testLogWriter was constructed here", so a third stream, or a
// new spawn path, is covered without being added to a list.
func TestKeeperLogWritersAreDetached(t *testing.T) {
	writers := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, name := range logWriterVars(fn.Body) {
				writers++
				if !callsMethodOn(fn.Body, name, "stop") {
					t.Errorf("%s: %s builds the log writer %s and never calls %s.stop(). "+
						"When the process it feeds outlives the test — the Kill branch of "+
						"teardown always does — the next write lands on a finished "+
						"*testing.T and panics a run that had already passed.",
						fset.Position(fn.Pos()), fn.Name.Name, name, name)
				}
			}
		}
	})
	if writers == 0 {
		t.Fatalf("no testLogWriter is constructed anywhere in this package. Either the " +
			"keeper's output is no longer forwarded to the test — in which case a failing " +
			"keeper says nothing at all — or the type was renamed and this guard checks " +
			"nothing.")
	}
}

// logWriterVars returns the names of variables assigned a testLogWriter.
func logWriterVars(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if i >= len(as.Lhs) || !buildsLogWriter(rhs) {
				continue
			}
			if id, ok := as.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
				out = append(out, id.Name)
			}
		}
		return true
	})
	return out
}

func buildsLogWriter(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	id, ok := lit.Type.(*ast.Ident)
	return ok && id.Name == "testLogWriter"
}

// TestKeeperConfigsCarryAPerStackIssuer — the rendered config's `iss` is this
// stack's, not a constant.
//
// Before NIM-469 every stack in the suite wrote `keeper-test-01`, so nothing on
// the wire or in a failure report said which of forty stacks a keeper belonged
// to; that is the confusion assertOwnKeeper now exists to break. A mutation put
// the literal back in the template and every test stayed green, because nothing
// read the template.
//
// The property is about the template text, so that is what is read: whatever
// follows `issuer:` in any config this package renders must be a format verb.
// The value it is fed comes from assignIdentity, which the second half checks is
// actually per-stack.
func TestKeeperConfigsCarryAPerStackIssuer(t *testing.T) {
	issuerLine := regexp.MustCompile(`(?m)^\s*issuer:\s*(\S+)`)
	templates := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			m := issuerLine.FindStringSubmatch(lit.Value)
			if m == nil {
				return true
			}
			templates++
			if !strings.HasPrefix(m[1], "%") {
				t.Errorf("%s: a keeper config template pins issuer to the literal %q. Every "+
					"stack in the run then signs and pins the same identity, and a token "+
					"that reaches the wrong keeper is rejected the same way a genuine auth "+
					"regression is — the ambiguity NIM-469 removed.",
					fset.Position(lit.Pos()), m[1])
			}
			return true
		})
	})
	if templates == 0 {
		t.Fatalf("no config template in this package sets an issuer. The keeper's `iss` is " +
			"then whatever its own default is, identically for every stack in the suite.")
	}

	first := (&Stack{}).assignIdentity("keeper-test", "127.0.0.1:34001")
	second := (&Stack{}).assignIdentity("keeper-test", "127.0.0.1:34002")
	if first == second || first == "" {
		t.Fatalf("assignIdentity produced %q for two different addresses. The port is the "+
			"source of uniqueness precisely because the kernel guarantees no two live "+
			"stacks hold the same one; without it the template's verb renders a constant.",
			first)
	}
}

// TestContainerTeardownReportsFailure — a container that refuses to die leaves a
// trace.
//
// The three teardown sites used to discard the error (`_ = c.Terminate(ctx)`),
// which is why "do containers survive a run?" had no answer anywhere in a log:
// a container that stayed up left no record, and every later test simply ran
// against a busier daemon. That is the same feedback loop daemonhealth.go is
// about, seen from the other end. A mutation restored the discard.
func TestContainerTeardownReportsFailure(t *testing.T) {
	sites := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			terminates := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok {
					for i, rhs := range as.Rhs {
						if !exprCallsMethod(rhs, "Terminate") {
							continue
						}
						if i < len(as.Lhs) {
							if id, ok := as.Lhs[i].(*ast.Ident); ok && id.Name == "_" {
								t.Errorf("%s: %s discards the error from Terminate. A "+
									"container that refuses to die then leaves no trace at "+
									"all, and the next thirty-nine tests share a daemon "+
									"with it — invisibly, which is what makes an "+
									"unreproducible red unreproducible.",
									fset.Position(as.Pos()), fn.Name.Name)
							}
						}
					}
				}
				if call, ok := n.(*ast.CallExpr); ok && exprCallsMethod(call, "Terminate") {
					terminates = true
				}
				return true
			})
			if !terminates {
				continue
			}
			sites++
			if !callsMethod(fn.Body, "Logf") && !callsMethod(fn.Body, "Errorf") {
				t.Errorf("%s: %s terminates a container but never logs. Teardown must not "+
					"fail a test — a teardown problem is not a verdict on the code — but it "+
					"has to stop being invisible.", fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	})
	if sites == 0 {
		t.Fatalf("nothing in this package terminates a container. The stands would then " +
			"outlive every test that raised them, and the daemon would carry all forty " +
			"runs' worth by the end of a suite.")
	}
}

// TestRaisedContainersAreAdoptedBeforeTheErrorCheck — a container is taken
// ownership of the moment it exists, not once it is known to be healthy.
//
// NIM-533, and the reason L3a could not produce two identical runs. testcontainers
// returns a LIVE container alongside its error — its own source says so, "at this
// point c might not be nil, give the caller an opportunity to call Destroy" — so
// the natural-looking
//
//	c, err := postgres.Run(ctx, image, opts...)
//	if err != nil { return fmt.Errorf(...) }
//	s.containers = append(s.containers, c)
//
// drops a running container on every failed bring-up. Nothing tears it down:
// ryuk reaps at process exit, not per test (NIM-532), so it stays for the rest
// of the suite, and the daemon each later test shares is slower than the one
// before it. That is a positive feedback loop, and it produces exactly L3a's
// signature — two or three failures per run, a different two or three each time,
// none reproducible alone.
//
// So the check is about ORDER, which no runtime test can see: adoption must
// happen before the error is checked. The subject is derived from the sources
// (containerRaisers, waitstrategy_test.go), so a fourth stand is covered without
// being named here.
func TestRaisedContainersAreAdoptedBeforeTheErrorCheck(t *testing.T) {
	raisers := containerRaisers(t)
	if len(raisers) == 0 {
		t.Fatalf("no function in this package raises a container. Either the stands come " +
			"up some other way now, or the constructor names moved and this guard — like the " +
			"table checks next to it — is looking at nothing.")
	}
	for _, r := range raisers {
		fn := funcAt(t, r.pos)
		if fn == nil {
			t.Errorf("%s: could not locate the function around the %s call", r.pos, r.fn)
			continue
		}
		if !adoptsBeforeReturning(fn.Body) {
			t.Errorf("%s: %s checks the error from the container constructor before adopting "+
				"the container. testcontainers hands back a RUNNING container together with "+
				"the error, so this returns with it still up and nothing holding a reference "+
				"— it survives until the process exits, and every test after this one shares "+
				"a daemon with it. That is the mechanism behind a suite that never gives the "+
				"same answer twice.", r.pos, fn.Name.Name)
		}
	}
}

// adoptsBeforeReturning walks each block, finds the statement that raises the
// container, and insists an adoptContainer call appears before the first error
// check that returns.
func adoptsBeforeReturning(body *ast.BlockStmt) bool {
	ok := false
	ast.Inspect(body, func(n ast.Node) bool {
		block, isBlock := n.(*ast.BlockStmt)
		if !isBlock {
			return true
		}
		raised := false
		for _, st := range block.List {
			if !raised {
				raised = raisesContainer(st)
				continue
			}
			if callsMethod(st, "adoptContainer") {
				ok = true
				return true
			}
			if ifStmt, isIf := st.(*ast.IfStmt); isIf && returnsInside(ifStmt) {
				return true // the error check came first
			}
		}
		return true
	})
	return ok
}

// raisesContainer — the same three constructor names containerRaisers derives
// the subject from, asked of a single statement.
func raisesContainer(st ast.Stmt) bool {
	for _, ctor := range []string{"Run", "RunContainer", "GenericContainer"} {
		if callsMethod(st, ctor) {
			return true
		}
	}
	return false
}

func returnsInside(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if _, ok := n.(*ast.ReturnStmt); ok {
			found = true
		}
		return true
	})
	return found
}

// funcAt returns the function declaration containing pos.
func funcAt(t *testing.T, pos token.Position) *ast.FuncDecl {
	t.Helper()
	var out *ast.FuncDecl
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		if base != filepathBase(pos.Filename) {
			return
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fset.Position(fn.Pos()).Line <= pos.Line && pos.Line <= fset.Position(fn.End()).Line {
				out = fn
			}
		}
	})
	return out
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// TestStandTeardownIsBoundToTheTest — teardown is registered on the *testing.T,
// not left to the caller.
//
// NIM-549. Every test in the tier writes `defer stack.Cleanup()`, and none of
// that helps: the defer is registered on the value the constructor RETURNS, and
// a constructor that fatals never returns one. NewStack fatals in at least six
// places after the containers are up — IssueKeeperServerCert, InitVaultTestSecrets,
// runKeeperInit, RegisterSoulPreAuth among them — and each of those leaked the
// whole stand: three containers and, past the keeper start, a subprocess holding
// a loopback port. On this tier that is not untidiness. The suite keeps going,
// so the next thirty-nine tests share a daemon with the wreckage, which is the
// mechanism behind "passes alone, fails in company" (NIM-533).
//
// t.Cleanup and not another runCleanups() at each fatal, because the property
// has to survive the fatal someone adds next year: it runs on Goexit, so it
// covers the whole dynamic extent including helpers this file has never heard
// of. The argument is checked, not just the call — `t.Cleanup(func(){})` is the
// shape this guard would otherwise accept while nothing gets torn down.
func TestStandTeardownIsBoundToTheTest(t *testing.T) {
	constructors := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !returnsStack(fn) {
				continue
			}
			constructors++
			if !registersStackCleanup(fn.Body) {
				t.Errorf("%s: %s returns a *Stack but never registers t.Cleanup(<stack>.Cleanup). "+
					"The caller's `defer stack.Cleanup()` cannot cover this function — a "+
					"t.Fatalf in here returns no stack to defer on — so every fatal path "+
					"between the first container and the return leaks the whole stand onto "+
					"the daemon the rest of the suite shares.",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	})
	if constructors < 2 {
		t.Fatalf("found %d function(s) returning a *Stack; NewStack and NewMultiKeeperStack "+
			"both do. Either a constructor was renamed out of reach of this guard or the "+
			"signature changed, and in either case it is now checking an empty set.",
			constructors)
	}
}

// registersStackCleanup insists the argument is a stack's Cleanup method value,
// not merely that t.Cleanup was called with something.
func registersStackCleanup(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Cleanup" {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if ok && arg.Sel.Name == "Cleanup" {
			found = true
		}
		return true
	})
	return found
}

// TestReaperFailureIsNamedAsTheDependency — a stand that failed because ryuk did
// is not reported as a fact about that stand's image.
//
// NIM-532. testcontainers raises its own reaper before our container and waits
// for it on a hardcoded 60s (reaper.go — no WithStartupTimeout, no env override),
// so nothing in this harness governs that wait and no budget here explains it.
// The failure still arrives through the stand's constructor, though, which means
// the pre-NIM-532 wording pinned it on the layer that was never involved: by the
// time probeDaemon() runs, a full minute after the contention, the daemon is
// usually idle again and the verdict read "the image or its configuration is what
// did not become ready".
//
// Behavioural rather than AST: the subject is the sentence a person reads, so the
// test reads it too. The second half is the known-bad the first half needs — an
// ordinary container failure must still be called a container failure, or a guard
// that only ever sees the reaper case would pass just as happily on a branch that
// swallowed every stand into it.
func TestReaperFailureIsNamedAsTheDependency(t *testing.T) {
	s := &Stack{t: t}
	healthy := daemonVerdict{ping: time.Millisecond}

	// The chain testcontainers actually produces: docker.go wraps every reaper
	// path with "reaper: ", reuseOrCreate adds "new reaper: ", and the innermost
	// text is the container wait giving up.
	//
	// The innermost text is externalCheck's, quoted in the shape that function
	// actually prints (wait/host_port.go:263) — "check target: retries: N
	// address: H:P: <inner>". An earlier version of this fixture said "container
	// did not become ready: get state: context deadline exceeded: retries: 536",
	// which appears nowhere in testcontainers-go v0.43.0; a fixture the library
	// cannot produce gates whatever it likes without gating this report.
	reaperErr := fmt.Errorf("reaper: %w", fmt.Errorf("new reaper: %w",
		errors.New("wait until ready: external check: check target: retries: 536 "+
			"address: localhost:33791: dial tcp 127.0.0.1:33791: connect: connection refused")))

	got := s.describeStandFailure(context.Background(), "postgres", reaperErr, healthy).Error()
	switch {
	case strings.Contains(got, "read this as the CONTAINER"):
		t.Errorf("a reaper startup failure is reported as a fact about postgres's image:\n%s\n\n"+
			"The reaper is testcontainers' own container and its 60s wait is unreachable "+
			"from here; blaming the stand sends the reader to look at an image that was "+
			"never the problem.", got)
	case !strings.Contains(got, "ryuk") || !strings.Contains(got, "NIM-532"):
		t.Errorf("a reaper startup failure is not named as one:\n%s\n\n"+
			"It has to say which thing failed and where the 60s comes from, or the reader "+
			"is left to recognise `retries: 536` on their own.", got)
	}

	// And it has to be reached on a contended daemon too, which is the ordering
	// the branch's own comment claims. "The machine was busy" is true there and
	// still the wrong thing to print: it sends the reader to rerun on an idle box
	// instead of telling them the 60s is in the dependency and not theirs to move.
	contended := daemonVerdict{err: errors.New("i/o timeout")}
	got = s.describeStandFailure(context.Background(), "postgres", reaperErr, contended).Error()
	if !strings.Contains(got, "ryuk") {
		t.Errorf("on a contended daemon the reaper failure is absorbed into the machine "+
			"verdict:\n%s\n\nContention is what causes it, so this is the common case, not "+
			"the exotic one.", got)
	}
	// Naming ryuk is necessary and not sufficient. Winning the switch means the
	// machine verdict is now unreachable for this stand, and on one contended run
	// the same root cause reaches this function as DEPENDENCY for one stand and
	// MACHINE for its neighbour — so a report that mentions only the reaper reads
	// as two unrelated problems. The addendum is what makes them one.
	if !strings.Contains(got, "ALSO not keeping up") {
		t.Errorf("the reaper branch won the switch on a contended daemon and said nothing "+
			"about the contention:\n%s\n\nThe machine verdict is unreachable once this branch "+
			"is taken, so whatever it does not say here is not said anywhere.", got)
	}

	ordinary := errors.New("container did not become ready: exited with code 1")
	got = s.describeStandFailure(context.Background(), "postgres", ordinary, healthy).Error()
	if !strings.Contains(got, "read this as the CONTAINER") {
		t.Errorf("a plain container failure on a healthy daemon is no longer called one:\n%s\n\n"+
			"The reaper branch is meant to carve out one specific dependency, not to become "+
			"the answer for every stand that will not start.", got)
	}
}

// TestDaemonProbeCannotBeHeldByTheDaemon — the probe answers on its own budget
// even when the docker call it makes never returns.
//
// NIM-533, and the reason this file grew a seam. The probe's budget used to be a
// context deadline, which reads correct and is not: testcontainers resolves the
// docker host inside a sync.Once that it enters with context.Background()
// (internal/core/docker_host.go, provider.go:144), so the first docker call in a
// process is unbounded and every later one waits on that Once. Against a daemon
// that accepts connections and never replies, the declared 15s was a promise the
// code could not keep — the observed cost was a whole `panic: test timed out
// after 8m0s` with no layer named, which is the exact symptom this ticket is
// about.
//
// The known-bad is a docker call that never replies, and it is not producible on
// demand against a real daemon — hence dockerPing being a variable. Three claims,
// because they fail separately: a healthy daemon must still read healthy, a
// silent one must be given up on near the budget, and the process must then say
// so at once rather than re-buying the same discovery per stand.
func TestDaemonProbeCannotBeHeldByTheDaemon(t *testing.T) {
	restore := dockerPing
	t.Cleanup(func() {
		dockerPing = restore
		// daemonWedged is process-wide and permanent by design, which is right
		// for a run that has genuinely lost docker and wrong to leave behind
		// here: every stand after this test would refuse to start, citing a
		// daemon this guard invented.
		daemonWedged.Store(false)
	})

	const budget = 200 * time.Millisecond

	// Negative control first, and before anything is poisoned: a probe that
	// reports silence unconditionally would satisfy every other claim below.
	dockerPing = func(_ context.Context, bounded func()) error { bounded(); return nil }
	if v := probeDaemonWithin(budget); v.err != nil || v.unresponsive() {
		t.Fatalf("a daemon that answers immediately is reported as %s. Read the other half of "+
			"this guard as vacuous until this passes: a probe that always says the daemon is "+
			"gone would ground the tier on a healthy box.", v)
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// The wedged shape, exactly: the call is entered and never returns, and the
	// context it was handed does not reach it. bounded is never called, because
	// in the real function nothing before that line can be stopped by ctx.
	dockerPing = func(_ context.Context, _ func()) error {
		<-release
		return nil
	}

	type outcome struct {
		v     daemonVerdict
		spent time.Duration
	}
	probe := func() outcome {
		t.Helper()
		done := make(chan outcome, 1)
		go func() {
			started := time.Now()
			v := probeDaemonWithin(budget)
			done <- outcome{v, time.Since(started)}
		}()
		select {
		case got := <-done:
			return got
		case <-time.After(10 * time.Second):
			t.Fatalf("probeDaemonWithin(%s) has not returned after 10s against a docker call "+
				"that never replies. That is the whole defect: the budget is not enforceable "+
				"through a context, because the call is parked in a sync.Once that ignores "+
				"one. It has to be raced against a timer.", budget)
			return outcome{}
		}
	}

	first := probe()
	if !errors.Is(first.v.err, errDaemonUnresponsive) {
		t.Errorf("a docker call that never replies produced %q, not the unresponsive verdict. "+
			"This is the case that costs a run its entire timeout, and it has to be named as "+
			"itself — a setup error (no socket, bad DOCKER_HOST) is a different report and "+
			"arrives in milliseconds.", first.v)
	}
	if first.spent < budget {
		t.Errorf("the probe gave up after %s, under its own %s budget. A daemon that is merely "+
			"slow would then be declared dead, and the tier would refuse to run on any loaded "+
			"box.", first.spent.Round(time.Millisecond), budget)
	}

	second := probe()
	if !errors.Is(second.v.err, errDaemonWedgedEarlier) {
		t.Errorf("after a probe timed out, the next one reported %q instead of saying the "+
			"process is finished with docker. The goroutine that timed out still holds "+
			"testcontainers' docker-host Once, so no docker call in this process can complete "+
			"— re-probing per stand spends the budget again to rediscover that, which across "+
			"this tier is minutes of waiting for a known answer.", second.v)
	}
	if second.spent >= budget {
		t.Errorf("the second probe still took %s. It is answering from a fact already "+
			"established, so it should not be paying for it twice.",
			second.spent.Round(time.Millisecond))
	}
}

// TestASlowPingDoesNotDeclareDockerUnusable — the other half of the guard above,
// and the one it was missing.
//
// The latch turns one timed-out probe into a refusal for every stand left in the
// run. That is right when the call is somewhere no deadline reaches, and a lie
// when the daemon simply answered late: the ping is bound by the context it is
// given, so it ends on its own and the next probe is free to find a daemon that
// came back. Latching on both is the same defect this file is about — naming the
// machine with confidence on evidence that does not support it — except now the
// harness is the one doing it, and it grounds the tier for the rest of the run.
//
// Reachable only because the probe stopped riding a memoised call. While it rode
// Health, a box healthy enough to reach the bounded phase had a warm cache and
// never timed out again, so this case could not occur.
func TestASlowPingDoesNotDeclareDockerUnusable(t *testing.T) {
	restore := dockerPing
	t.Cleanup(func() {
		dockerPing = restore
		daemonWedged.Store(false)
	})

	const budget = 200 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	// A call that got past the docker-host lookup and then took longer than the
	// budget: a loaded daemon, not a wedged one.
	dockerPing = func(_ context.Context, bounded func()) error {
		bounded()
		<-release
		return nil
	}

	probe := func() daemonVerdict {
		t.Helper()
		done := make(chan daemonVerdict, 1)
		go func() { done <- probeDaemonWithin(budget) }()
		select {
		case v := <-done:
			return v
		case <-time.After(10 * time.Second):
			t.Fatalf("probeDaemonWithin(%s) never returned", budget)
			return daemonVerdict{}
		}
	}

	if v := probe(); !errors.Is(v.err, errDaemonUnresponsive) {
		t.Fatalf("a ping that outran the budget reported %q rather than silence. Read the rest "+
			"of this guard as vacuous until this passes — it is checking what happens after "+
			"this verdict.", v)
	}

	// The daemon comes back. Nothing about the earlier sample may stop the
	// harness from finding that out.
	dockerPing = func(_ context.Context, bounded func()) error { bounded(); return nil }
	second := probe()
	if errors.Is(second.err, errDaemonWedgedEarlier) {
		t.Errorf("after one ping ran over its budget the harness declared docker unusable for "+
			"the rest of the process:\n  %s\n\nThe ping is the part of the call the budget does "+
			"reach, so it had already ended; every stand still to come is now refused, and each "+
			"refusal blames a machine that is answering.", second)
	}
	if second.err != nil || second.unresponsive() {
		t.Errorf("a daemon that recovered is still reported as %s. The verdict is being carried "+
			"over from the earlier probe rather than measured.", second)
	}
}

// TestALookupThatCompletesLateClearsTheLatch — the latch is a claim about a call
// that has not come back, and it has to stop being made when the call comes back.
//
// The timer gives up on the probe; it does not end the call. So the case here is
// real and not exotic: the docker-host lookup finishes shortly after the harness
// stopped waiting on it. Everything the latch asserts — no docker call in this
// process can complete — is false from that moment, and leaving it set refuses
// every remaining stand in the run while blaming a daemon that is answering.
//
// Deterministic because the marker is supplied by probeDaemonWithin and called by
// a substitutable function, so the test can place the completion after the timer
// rather than hope for it.
func TestALookupThatCompletesLateClearsTheLatch(t *testing.T) {
	restore := dockerPing
	t.Cleanup(func() {
		dockerPing = restore
		daemonWedged.Store(false)
	})

	const budget = 100 * time.Millisecond
	marked := make(chan struct{})
	dockerPing = func(_ context.Context, bounded func()) error {
		// Past the budget: by the time this runs the probe has given up and
		// recorded that it did.
		time.Sleep(budget * 3)
		bounded()
		close(marked)
		return nil
	}

	if v := probeDaemonWithin(budget); !errors.Is(v.err, errDaemonUnresponsive) {
		t.Fatalf("a call that had not reached its bounded phase by the budget reported %q "+
			"instead of silence; the rest of this guard checks what happens after that "+
			"verdict, so read it as vacuous until this passes.", v)
	}
	<-marked

	dockerPing = func(_ context.Context, bounded func()) error { bounded(); return nil }
	if v := probeDaemonWithin(budget); errors.Is(v.err, errDaemonWedgedEarlier) {
		t.Errorf("the docker-host lookup completed after the probe gave up, and the harness is "+
			"still saying:\n  %s\n\nThat sentence was only ever true while the lookup was "+
			"outstanding. Every stand left in the run is now refused over a fact that expired.", v)
	}
}

// TestTheProbeMarksWhereItsBudgetStartsApplying — the marker is what makes the
// distinction above observable, so its placement is a claim in its own right.
//
// Nothing in NewDockerProvider takes the context it is handed (provider.go:143
// enters both the docker-host Once and the first Info with context.Background()),
// and the Ping after it does. The marker sits exactly on that boundary. Moved up,
// a wedged daemon stops being recognised as one and every stand pays the full
// budget to rediscover it; moved down, a slow ping grounds the run. Neither
// misplacement changes what the function returns on a healthy box, which is why
// this is checked structurally.
func TestTheProbeMarksWhereItsBudgetStartsApplying(t *testing.T) {
	lit := harnessVarFuncLit(t, "dockerPing")

	if lit.Type.Params == nil || len(lit.Type.Params.List) < 2 || len(lit.Type.Params.List[1].Names) != 1 {
		t.Fatalf("dockerPing no longer takes a second named parameter. The probe reports the " +
			"boundary between the unstoppable part of the call and the part its budget covers " +
			"through that parameter; without it probeDaemonWithin cannot tell a hung call from " +
			"a slow one and has to guess.")
	}
	marker := lit.Type.Params.List[1].Names[0].Name

	var markerAt, providerAt, pingAt token.Pos
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == marker {
				markerAt = call.Pos()
			}
		case *ast.SelectorExpr:
			switch fn.Sel.Name {
			case "NewDockerProvider":
				providerAt = call.Pos()
			case "Ping":
				pingAt = call.Pos()
			}
		}
		return true
	})

	switch {
	case !markerAt.IsValid():
		t.Fatalf("dockerPing never calls %s. probeDaemonWithin then sees every timeout as a "+
			"call that cannot be stopped, so one slow ping latches the run into refusing "+
			"every remaining stand.", marker)
	case !providerAt.IsValid() || !pingAt.IsValid():
		t.Fatalf("dockerPing no longer calls both NewDockerProvider and Ping, so where the " +
			"boundary lies is no longer something this guard can check.")
	case markerAt < providerAt:
		t.Errorf("dockerPing calls %s before the provider exists. Everything up to that point "+
			"ignores the context, so a daemon that never replies would be reported as one that "+
			"was merely slow — and every stand after it would spend the whole budget finding "+
			"that out again.", marker)
	case markerAt > pingAt:
		t.Errorf("dockerPing calls %s after the ping. The ping is the part the budget can stop, "+
			"so a run that lost docker for one call would be told docker is unusable for the "+
			"rest of the process.", marker)
	}
}

// TestDaemonProbeRunsTheDockerCallOffTheCallersGoroutine — the racing is the
// mechanism, so it is checked structurally as well as behaviourally.
//
// NIM-533. The guard above proves the probe returns on time; this one proves it
// does so for the right reason. Inlining the call back onto the caller's
// goroutine leaves a function that still passes every fast-path test and reverts
// the fix entirely, and Go has nothing to say about it.
func TestDaemonProbeRunsTheDockerCallOffTheCallersGoroutine(t *testing.T) {
	fn := harnessFuncNamed(t, "probeDaemonWithin")

	var spawned []*ast.GoStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			spawned = append(spawned, g)
		}
		return true
	})

	// The names the ping can be invoked under: the variable, and anything bound
	// to it. It is deliberately read on the caller's goroutine and called through
	// a local — matching only the variable's own name would find nothing here and
	// report success.
	names := map[string]bool{"dockerPing": true}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			id, ok := rhs.(*ast.Ident)
			if !ok || !names[id.Name] || i >= len(as.Lhs) {
				continue
			}
			if lhs, ok := as.Lhs[i].(*ast.Ident); ok {
				names[lhs.Name] = true
			}
		}
		return true
	})

	calls := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !names[id.Name] {
			return true
		}
		calls++
		for _, g := range spawned {
			if call.Pos() > g.Pos() && call.End() < g.End() {
				return true
			}
		}
		t.Errorf("probeDaemonWithin calls %s on the caller's own goroutine. That call can be "+
			"unbounded — it is what enters testcontainers' docker-host sync.Once — so whatever "+
			"budget surrounds it here, the function returns when docker decides it does, and "+
			"the surrounding budget is decoration.", id.Name)
		return true
	})
	if calls == 0 {
		t.Fatalf("probeDaemonWithin no longer calls dockerPing at all; this guard is checking " +
			"nothing. Either the probe stopped talking to docker, or it found another way to " +
			"do it that this test cannot see.")
	}

	sel := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectStmt); ok && len(s.Body.List) >= 2 {
			sel = true
		}
		return true
	})
	if !sel {
		t.Errorf("probeDaemonWithin has no select with two arms. Moving the docker call to " +
			"another goroutine only helps if this one can finish without it — otherwise the " +
			"receive blocks exactly as long as the call did.")
	}
}

// TestStandBringUpAsksTheDaemonBeforeRaisingAnything — the pre-flight check.
//
// NIM-533. The post-mortem probe explains a bring-up that failed; it cannot
// explain one that never came back. A daemon that has stopped answering takes
// the first container call and keeps it, so the harness has to ask before it
// raises anything rather than after something fails.
//
// Positional, because that is the whole content of the claim: a probe placed
// after the loop is the code that already existed, and it is what produced an
// eight-minute unattributed timeout.
func TestStandBringUpAsksTheDaemonBeforeRaisingAnything(t *testing.T) {
	fn := harnessFuncNamed(t, "bringUpStand")

	startPos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "start" && !startPos.IsValid() {
			startPos = call.Pos()
		}
		return true
	})
	if !startPos.IsValid() {
		t.Fatalf("bringUpStand never calls its start function. This guard measures everything " +
			"else against that call, so it is now measuring against nothing.")
	}

	if preflightIf(fn.Body, startPos) == nil {
		t.Errorf("bringUpStand raises a stand without asking the daemon anything first, or asks " +
			"and does not return on the answer. Against a daemon that accepts connections and " +
			"never replies, the container call below never comes back: the probe further down " +
			"is never reached, no verdict is printed, and the run ends as `panic: test timed " +
			"out` naming no layer at all. That failure is what NIM-533 was filed for.")
	}
}

// TestStandBringUpRefusesOnlyOnSilence — the pre-flight is strictly narrower than
// the retry.
//
// NIM-533, and it is the branch most likely to be "tidied" into the predicate
// next to it. contended() is true for a daemon that is merely slow, and slow is
// the normal state of a CI box under a suite like this one. Refusing there would
// convert a tier that runs slowly into a tier that does not run, and it would do
// it while printing a sentence about the machine that is perfectly true.
func TestStandBringUpRefusesOnlyOnSilence(t *testing.T) {
	slow := daemonVerdict{ping: 3 * time.Second}
	if !slow.contended() {
		t.Fatalf("a %s ping is not counted as contention, so the two predicates this guard "+
			"separates no longer differ and it proves nothing.", slow.ping)
	}
	if slow.unresponsive() {
		t.Errorf("a daemon that answered in %s is classed as unresponsive. Nothing would then "+
			"distinguish a loaded box from a dead one, and the pre-flight would ground the "+
			"tier on the first busy machine it met.", slow.ping)
	}
	if !(daemonVerdict{err: errDaemonUnresponsive}).unresponsive() {
		t.Errorf("a daemon that gave no answer is not classed as unresponsive, which leaves " +
			"the pre-flight with no condition that ever fires.")
	}

	fn := harnessFuncNamed(t, "bringUpStand")
	startPos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "start" && !startPos.IsValid() {
			startPos = call.Pos()
		}
		return true
	})
	pre := preflightIf(fn.Body, startPos)
	if pre == nil {
		t.Skip("no pre-flight check to inspect; TestStandBringUpAsksTheDaemonBeforeRaisingAnything " +
			"is the guard reporting that")
	}
	if callsMethod(pre.Cond, "contended") {
		t.Errorf("the pre-flight refuses on contended(). That predicate is true whenever the " +
			"daemon is slow, and the retry below exists precisely to ride that out — so this " +
			"aborts the stands the retry was written to save, before either gets a chance.")
	}
	if !callsMethod(pre.Cond, "unresponsive") {
		t.Errorf("the pre-flight no longer decides on unresponsive(). Whatever it decides on " +
			"now, the distinction between a daemon that is slow and one that is gone has left " +
			"the code, and that distinction is the entire content of the check.")
	}
}

// preflightIf finds the guarded check that runs before the stand is raised: an if
// positioned ahead of the start call, deciding on a daemon probe, that returns.
//
// All three conditions, because dropping any one of them is a plausible edit that
// silently removes the protection — a probe whose verdict is only logged reads
// like diligence and changes nothing.
func preflightIf(body *ast.BlockStmt, startPos token.Pos) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || found != nil || ifs.Pos() > startPos {
			return true
		}
		// Init and Cond only, not the body: the subject is what the check DECIDES
		// on. A probe called inside the body has already let the decision be made
		// by something else.
		probed := nodeCallsIdent(ifs.Cond, "probeDaemon")
		if ifs.Init != nil {
			probed = probed || nodeCallsIdent(ifs.Init, "probeDaemon")
		}
		if !probed || !returnsInside(ifs.Body) {
			return true
		}
		found = ifs
		return true
	})
	return found
}

// nodeCallsIdent is callsIdent over any node — the pre-flight's probe sits in an
// if-statement's initialiser, which is not a block.
func nodeCallsIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			found = found || f.Name == name
		case *ast.SelectorExpr:
			found = found || f.Sel.Name == name
		}
		return !found
	})
	return found
}

// TestTheRetryIsSpentOnTheBoxTheCauseNames — NIM-646's second consequence, and
// the one that is not about wording.
//
// The report and the loop above it read the same failure and reached opposite
// conclusions. describeStandFailure printed "read this as the MACHINE … rerunning
// this test alone on an idle box is the check", and bringUpStand — deciding from
// the post-mortem probe alone — declined to spend the one attempt it had. The
// probe is taken after the failed attempt has been torn down, so a daemon that
// spent the whole wait behind a queue answers it in single-digit milliseconds and
// contended() is false. The retry built to ride out a loaded box was therefore
// never spent on one.
//
// The known-bad is the ticket's own shape: a docker call that failed inside the
// wait, next to a probe saying the daemon is fine. Every row below pairs a cause
// with that same healthy probe, so the probe cannot be what decides any of them.
func TestTheRetryIsSpentOnTheBoxTheCauseNames(t *testing.T) {
	healthy := daemonVerdict{ping: 8 * time.Millisecond}
	if healthy.contended() {
		t.Fatalf("the probe this table pairs with every cause is itself contended, so a row "+
			"that retries proves nothing about the cause: %s", healthy)
	}

	const wrapped = "postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: %d address: localhost:34142: %s"
	const deadline = `get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/` +
		`d8fd5e0b8a39/json": context deadline exceeded`

	// 600 rounds is half of standReadyTimeout at 100ms a round: below it the
	// wait cannot account for its own budget by sleeping, above it it can.
	for _, tc := range []struct {
		name  string
		cause error
		retry bool
		why   string
	}{
		{"a docker call that failed mid-wait", fmt.Errorf(wrapped, 12,
			"get state: Error response from daemon: internal server error"), true,
			"the call itself failed inside the window, which no later probe can see"},
		{"a count too small for the budget it burned", fmt.Errorf(wrapped, 12, deadline), true,
			"12 rounds sleep 1.2s of two minutes, so the rest went into the docker calls"},
		{"a count that accounts for the budget", fmt.Errorf(wrapped, 1087, deadline), false,
			"1087 answered inspections say the daemon kept up; there is no delay to ride out"},
		{"a container that exited", fmt.Errorf(wrapped, 3, "container exited with code 1"), false,
			"the code cannot say the box chose it, and a retry would hide an image that exits"},
		{"a container that was killed", fmt.Errorf(wrapped, 3, "container exited with code 137"),
			false, "something outside the run is acting on this daemon; a retry walks back into it"},
		{"a container the kernel took", fmt.Errorf(wrapped, 3,
			"container crashed with out-of-memory (OOMKilled)"), false,
			"a second attempt asks for the memory the box just proved it does not have"},
		{"a container being removed", fmt.Errorf(wrapped, 3,
			`unexpected container status "removing"`), false,
			"a reaper or a prune is running right now and will take the second one too"},
		{"a container already gone", fmt.Errorf(wrapped, 3,
			"get state: Error response from daemon: No such container: 9f2c1ae40b3d"), false,
			"the call was ANSWERED; load does not delete containers"},
		{"the reaper", fmt.Errorf("postgres container: run postgres: reaper: create container: "+
			"wait until ready: external check: check target: retries: 4 address: localhost:32901: "+
			"%s", deadline), false,
			"the stand is not what failed, and its envelope belongs to ryuk's address"},
		{"a cause with no wait envelope", errors.New("postgres container: run postgres: " +
			"create container: invalid reference format"), false,
			"nothing here was measured inside a wait, so nothing here names the box"},
	} {
		if got := machineDelayedTheStand(tc.cause, healthy); got != tc.retry {
			verb := "is not retried"
			if got {
				verb = "is retried"
			}
			t.Errorf("%s %s, and it should %sbe: %s\n\ncause: %v",
				tc.name, verb, map[bool]string{true: "", false: "not "}[tc.retry], tc.why, tc.cause)
		}
	}

	// The probe-side half is still whole. NIM-646 widened this decision; it must
	// not have narrowed it, or a daemon measurably struggling right now loses the
	// attempt it has always had.
	slow := daemonVerdict{ping: 3 * time.Second}
	if !slow.contended() {
		t.Fatalf("a %s ping is not counted as contention, so the check below proves nothing",
			slow.ping)
	}
	if !machineDelayedTheStand(fmt.Errorf(wrapped, 3, "container exited with code 1"), slow) {
		t.Errorf("a daemon answering in %s does not buy the retry any more. That is the "+
			"pre-NIM-646 behaviour, and widening this decision must not have removed it: the "+
			"cause is only a SECOND witness.", slow.ping)
	}
}

// TestTheRetryDecisionNamesEveryShapeTheReportNames — the two readers of one
// cause must not drift apart.
//
// machineDelayedTheStand cannot ask a single predicate whether the daemon was the
// problem, because none of them answers that. inPortWait is true for every cause
// carrying the port wait's count, and daemonStoppedAnswering means the daemon only
// on causes the branches ABOVE it in describeStandFailure have already declined —
// their verdicts come from the switch's ORDER. So the predicate restates that
// order as an exclusion list, and a list restating another list is exactly the
// thing that rots: a branch added to the report above daemonStoppedAnswering, and
// not added here, silently starts buying retries for a container the report is
// blaming. This fails when they stop matching.
//
// MEMBERSHIP, though, not order — and the name says so because the earlier one did
// not. The decision's own switch is deliberately in a different order from the
// report's (it answers a coarser question and short-circuits sooner), so demanding
// the same sequence would be demanding a defect. What is asserted is that every
// shape the report names appears somewhere in the decision; that the two agree on
// each shape's ANSWER is a different guard, the ten-row table above.
func TestTheRetryDecisionNamesEveryShapeTheReportNames(t *testing.T) {
	report := harnessFuncNamed(t, "describeStandFailure")

	var arms []string
	var found bool
	ast.Inspect(report.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag != nil || found {
			return !found
		}
		var seen []string
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok || clause.List == nil {
				continue
			}
			for _, cond := range clause.List {
				seen = append(seen, calledNames(cond)...)
			}
		}
		for _, name := range seen {
			if name == "daemonStoppedAnswering" {
				arms, found = seen, true
				return false
			}
		}
		return true
	})
	if !found {
		t.Fatalf("no switch in describeStandFailure reaches daemonStoppedAnswering, so this " +
			"guard has no order to mirror and is measuring nothing.")
	}
	if len(arms) < 8 {
		t.Fatalf("the switch this guard read has only %d branch(es) (%v). That is fewer than the "+
			"report is built from, so it is reading the wrong one.", len(arms), arms)
	}

	decision := harnessFuncNamed(t, "machineDelayedTheStand")
	mirrored := map[string]bool{}
	for _, name := range calledNames(decision.Body) {
		mirrored[name] = true
	}

	// EVERY arm, not only the ones above daemonStoppedAnswering. Those are what
	// the exclusions were written for, but a branch added lower down is the case
	// with no witness: machineDelayedTheStand ends in a default, so an unmirrored
	// branch does not fail to compile and does not fail a case here — it silently
	// takes whatever the default says and buys or refuses a retry on a shape
	// nobody classified. Naming all of them is what turns that into a red line.
	for _, name := range arms {
		if !mirrored[name] {
			t.Errorf("describeStandFailure has a %s branch and machineDelayedTheStand never "+
				"mentions it.\n\nEvery branch of that report is a verdict about WHO delayed the "+
				"stand, and the retry decision is the same question asked once more — so a shape "+
				"the report names and the decision does not is one that falls through a default "+
				"to an answer nobody chose for it. If this branch genuinely takes the default's "+
				"answer, say so by naming it in a case.\n\nreport order: %v", name, arms)
		}
	}
}

// calledNames — the function and method names called anywhere under this node,
// in source order and without duplicates.
func calledNames(node ast.Node) []string {
	var out []string
	seen := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		return true
	})
	return out
}

// argIdents — this call's arguments, one per position, with anything that is not
// a bare identifier rendered as "<expr>". Positional and lossy on purpose: the
// question it answers is which variables were handed over, and a caller that
// passes a literal or a call has already failed that question.
func argIdents(call *ast.CallExpr) []string {
	out := make([]string, 0, len(call.Args))
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok {
			out = append(out, id.Name)
			continue
		}
		out = append(out, "<expr>")
	}
	return out
}

// TestTheRetryIsAnnouncedByTheWitnessThatBoughtIt — the retry decision reads two
// witnesses, and the line that announces it used to print only one.
//
// Before the cause could buy an attempt, the probe was the only thing that ever
// did, so printing the probe was printing the reason. Widening the decision left
// that line behind, and the two disagree in the ordinary case: a docker call that
// dies inside the wait leaves a daemon that answers in milliseconds a second
// later, so the log read "the retry was bought because the daemon answered in 8ms
// — it is healthy". That is not a weaker explanation, it is an argument against
// the decision it is explaining, printed by the code that made it.
//
// Gated as a unit test on the witness rather than through bringUpStand, which
// needs a live daemon and two failed bring-ups to reach the same string.
func TestTheRetryIsAnnouncedByTheWitnessThatBoughtIt(t *testing.T) {
	healthy := daemonVerdict{ping: 8 * time.Millisecond}
	cause := errors.New("postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: 12 address: localhost:34142: get state: EOF")

	if !machineDelayedTheStand(cause, healthy) {
		t.Fatalf("this cause no longer buys a retry over a healthy probe, so the disagreement "+
			"this guard is about cannot arise and it is measuring nothing: %v", cause)
	}

	got := retryWitness(cause, healthy)
	switch {
	case !strings.Contains(got, "get state: EOF"):
		t.Errorf("the retry is announced without the evidence that bought it:\n%s\n\nThe probe "+
			"did not buy this one — it says the daemon is fine. The cause did, and a reader "+
			"given the other witness is being told the attempt was spent for no reason.", got)
	case !strings.Contains(got, "measured inside the window that failed"):
		t.Errorf("the announcement does not say which window its evidence comes from:\n%s\n\n"+
			"Both witnesses are in this line and they were taken a second apart. Which one was "+
			"inside the failure is the whole difference between them.", got)
	case !strings.Contains(got, "not the witness here"):
		t.Errorf("the probe is printed beside the cause with nothing marking which decided:\n"+
			"%s\n\nA healthy probe sitting next to a verdict that spent an attempt reads as the "+
			"reason for it unless the line says it is not.", got)
	}

	// The other witness, so the branch is a fork rather than one arm and a dead
	// one. A contended probe buys the retry whatever the cause says, and there the
	// probe IS the reason.
	loaded := daemonVerdict{ping: 3 * time.Second}
	if !loaded.contended() {
		t.Fatalf("a %s ping is not contention, so the case below never enters that arm.", loaded.ping)
	}
	if got := retryWitness(cause, loaded); !strings.Contains(got, "the probe taken right after") {
		t.Errorf("a retry bought by a struggling daemon is credited elsewhere:\n%s\n\nThe probe "+
			"is checked ahead of every cause and decides on its own, so it is what has to be "+
			"named here.", got)
	}

	// And the announcements have to go through it. Both are inside bringUpStand,
	// which no unit test reaches, so the assertions above gate a function that is
	// free to have no callers.
	loop := harnessFuncNamed(t, "bringUpStand")
	announcements, viaWitness := 0, 0
	ast.Inspect(loop.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// Walked rather than type-asserted: both format strings are concatenations
		// of literals, so the marker sits in the first operand of a BinaryExpr and
		// an assertion to *ast.BasicLit finds neither line.
		tagged := false
		ast.Inspect(call.Args[0], func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && strings.Contains(lit.Value, "[stand-retry]") {
				tagged = true
			}
			return !tagged
		})
		if !tagged {
			return true
		}
		announcements++
		// Its ARGUMENTS too, not just its name. retryWitness(nil, lastVerdict)
		// compiles, keeps this count intact, and ships a line ending "…which is why
		// it is not the witness here): <nil>" — the announcement naming a witness
		// the attempt never had. The two the loop carries are the two it must get.
		//
		// The ceiling is the CALL, not the value: this reads the identifiers at the
		// call site, so `lastErr = nil` on the line above passes it unchanged. That
		// is not a gap this guard can close — an AST cannot evaluate — and it is
		// stated here so the next reader does not take "witness the attempt never
		// had" as closed. Nothing in this tier closes it. The value depends on the
		// loop keeping one assignment per attempt (daemonhealth.go:587), and no unit
		// test runs the loop — that is the same absence the paragraph above gives as
		// the reason these announcements are gated through the syntax at all. What
		// exercises it is a real retry in the tier this harness serves.
		named := false
		ast.Inspect(call, func(n ast.Node) bool {
			inner, ok := n.(*ast.CallExpr)
			if !ok || named {
				return !named
			}
			if id, ok := inner.Fun.(*ast.Ident); !ok || id.Name != "retryWitness" {
				return true
			}
			named = true
			viaWitness++
			if args := argIdents(inner); strings.Join(args, ", ") != "lastErr, lastVerdict" {
				t.Errorf("a [stand-retry] line calls retryWitness(%s), and the witnesses this "+
					"loop carries are lastErr and lastVerdict.\n\nAnything else compiles, keeps "+
					"this guard green, and prints a witness the attempt did not have.",
					strings.Join(args, ", "))
			}
			return false
		})
		return true
	})
	if announcements < 2 {
		t.Fatalf("found %d [stand-retry] line(s) in bringUpStand, expected the two it prints "+
			"(one when the retry works, one when it is spent). This guard is reading the wrong "+
			"function or the announcements moved.", announcements)
	}
	if viaWitness != announcements {
		t.Errorf("%d of %d [stand-retry] line(s) name their witness through retryWitness.\n\n"+
			"One that formats a verdict itself is the defect this guard exists for: it compiles, "+
			"it prints something plausible, and on the ordinary docker-call failure it names the "+
			"witness that did not decide.", viaWitness, announcements)
	}
}

// TestTheBringUpLoopDecidesTheRetryThroughThatPredicate — without this the table
// above gates a function nobody calls.
//
// The predicate is pure and testable precisely because it was lifted out of the
// loop, and that is also how it becomes dead code: restore `!lastVerdict.
// contended()` at the break and every assertion above still passes while the
// behaviour they describe is gone. The break is the whole mechanism, so the guard
// checks the break.
func TestTheBringUpLoopDecidesTheRetryThroughThatPredicate(t *testing.T) {
	fn := harnessFuncNamed(t, "bringUpStand")

	var gated bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || len(stmt.Body.List) != 1 {
			return true
		}
		if br, ok := stmt.Body.List[0].(*ast.BranchStmt); !ok || br.Tok != token.BREAK {
			return true
		}
		for _, name := range calledNames(stmt.Cond) {
			if name == "machineDelayedTheStand" {
				gated = true
				return false
			}
		}
		return true
	})

	if !gated {
		t.Errorf("nothing in bringUpStand leaves the retry loop on machineDelayedTheStand. " +
			"Whatever that predicate now says, the loop is deciding the retry some other way — " +
			"and the table that checks it is checking a function with no caller.")
	}
}

// TestDaemonProbeDoesNotRideAMemoisedCall — the probe has to reach the daemon on
// EVERY call, not merely the first one in the process.
//
// testcontainers caches client.Info in package variables (dockerInfo,
// dockerInfoSet, dockerInfoLock in docker_client.go), and DockerProvider.Health
// is exactly that Info. A probe built on Health does one real round trip per
// binary: after the first success it returns nil in microseconds forever, and
// contended(), the pre-flight in bringUpStand and describeStandFailure all go on
// reasoning from a measurement nobody took.
//
// Checked on the source rather than by timing, because the defect is structurally
// invisible in the condition this tier has actually been run under. Against a
// daemon that is wedged from the start Info never succeeds, the cache never
// warms, and Health stays honest — the bug appears only on a HEALTHY daemon,
// which is the one machine a harness written for red runs never sees. That is
// how it survived three identical runs and a full mutation battery.
func TestDaemonProbeDoesNotRideAMemoisedCall(t *testing.T) {
	lit := harnessVarFuncLit(t, "dockerPing")

	for _, memoised := range []string{"Health", "Info"} {
		if callsMethod(lit.Body, memoised) {
			t.Errorf("dockerPing calls %s. testcontainers caches client.Info process-wide "+
				"(dockerInfo/dockerInfoSet in docker_client.go) and Health IS that Info, so after "+
				"one success this probe stops touching the daemon: the ping reads as microseconds "+
				"and the error as nil whatever the daemon is doing. Everything downstream then "+
				"reasons from a measurement nobody took — contended() can never fire, the "+
				"pre-flight can never catch a daemon that dies mid-run, and a saturated daemon is "+
				"reported as the CONTAINER.", memoised)
		}
	}
	if !callsMethod(lit.Body, "Ping") {
		t.Errorf("dockerPing no longer calls Ping. The probe has to be a call the library does " +
			"not cache — /_ping goes to the daemon every time — or the durations this whole file " +
			"reports are the durations of a map lookup.")
	}
}

// TestAWaitThatKeptGettingAnswersDoesNotCondemnTheImage — the report may not
// promise that a red will survive a rerun when its own evidence cannot know.
//
// NIM-646. Three live L3a runs out of three, on two different images, ended with
// "read this as the CONTAINER: … This one does not go away on a rerun" — and all
// three went away on a rerun, in 9.5-58s. The verdict was defensible; the
// sentence after it was not. A container that is up and starved of CPU produces
// the same failure as one that is broken, and the harness has no input that
// separates them, so it must not assert the difference.
//
// What it does have is the count AND the budget. testcontainers' externalCheck
// inspects the container at the top of every round, dials the port after it and
// sleeps 100ms on a refusal, so `retries: 977` is 977 answered inspections, 977
// refused dials and 97.7s of sleep — inside a window whose length is known,
// because the deadline that ended it is this stand's own. Sleeping for most of
// the budget is what leaves the daemon no room to have been slow; the count on
// its own says nothing, which is the next guard down.
//
// It is also why the tempting fix is wrong: the wait EXITS through a docker call,
// so the last thing this string shows is a docker request that ran out of time on
// a daemon that had just answered a thousand of them. Read that as the daemon and
// the misattribution is inverted, not fixed.
//
// And the refusals are not the container's port. externalCheck dials
// target.Host() plus the MAPPED port, and ForAll runs strategies in sequence — so
// on postgres, whose log strategy (occurrence 2) runs before the port strategy,
// the server had already announced itself inside the container while this box was
// still refusing connections on the published address. "The image never listened"
// is one of three shapes that produce this string, which is why the report has to
// name the tail rather than pick one.
//
// The two known-bads here are the ways this claim can be passed without being
// true, and both are containers the inspection found GONE: they keep the
// categorical sentence, because it is true about them and deleting it everywhere
// is the cheap way to pass the first assertion. Everything else that shares this
// envelope is the guard below.
func TestAWaitThatKeptGettingAnswersDoesNotCondemnTheImage(t *testing.T) {
	s := &Stack{t: t}
	healthy := daemonVerdict{ping: 8 * time.Millisecond}
	if healthy.contended() {
		t.Fatalf("an %s ping already counts as contention, so every claim below is decided "+
			"before the code under test is reached.", healthy.ping)
	}

	// The cause from run 3 on 2026-08-09, shortened only in the container id.
	starved := fmt.Errorf("postgres container: run postgres: wait until ready: external check: "+
		"check target: retries: 977 address: localhost:34142: get state: "+
		`Get "http://%%2Fvar%%2Frun%%2Fdocker.sock/v1.54/containers/801bcaa0/json": %w`,
		context.DeadlineExceeded)

	got := s.describeStandFailure(context.Background(), "postgres", starved, healthy).Error()
	switch {
	case !strings.Contains(got, "passing alone means the machine"):
		t.Errorf("a stand that lost 977 dials to a refused port is not handed the check that "+
			"separates the two readings:\n%s\n\nNothing here can tell a broken image from a box "+
			"too loaded to let postgres finish initdb inside the budget, so the report has to "+
			"name the experiment instead of asserting its outcome. Three runs asserted the "+
			"outcome and a solo rerun broke the promise three times, in 9.5-58s.", got)
	// On the sentence and not on "977": the cause is echoed at the top of every
	// report, so the bare number is present whatever this function does with it.
	// An assertion that reads the echo passes on a build that dropped the fact
	// line entirely — checked by mutation, it survived.
	case !strings.Contains(got, "977 container inspections the daemon ANSWERED"):
		t.Errorf("the report drops the only measurement taken inside the failing window:\n%s\n\n"+
			"977 answered inspections is what rules the daemon out; without it the reader is "+
			"back to the probe, which ran a minute later and says the daemon is fine now.", got)
	// The spans, as literals. They are 977×100ms and what is left of a 2m budget
	// after it, and writing them as the constants that produced them would make
	// this assertion true for any poll interval and any budget — the shape that
	// has no ceiling. If standReadyTimeout or waitPollInterval moves, this goes
	// red, and it should: the sentence is arithmetic about those two numbers.
	case !strings.Contains(got, "sleeps come to 1m38s of the 2m0s"):
		t.Errorf("the report no longer weighs the wait's own sleeps against the budget:\n%s\n\n"+
			"977 rounds is 1m38s of a 2m0s window. That fraction is the whole claim — the count "+
			"alone is equally the signature of a daemon taking ten seconds per call.", got)
	case !strings.Contains(got, "fit in the 22s that is left"):
		t.Errorf("the report states the sleeps and never says what that leaves:\n%s\n\n"+
			"22s for 977 inspections, 977 dials and everything before them is what a daemon that "+
			"was keeping up looks like. Without the remainder the reader has half a fraction.", got)
	case !strings.Contains(got, "not the thing that was stuck"):
		t.Errorf("the count is printed and never used to rule anything out:\n%s\n\nA number the "+
			"reader has to interpret alone is the state this tier was in before NIM-533.", got)
	// Ambiguity without a discriminator is the categorical sentence wearing a
	// hedge, so the report has to name one — but only one it actually printed.
	// This Stack has no container registered, so its report says "container: never
	// created", and a sentence sending the reader to a log tail that is not there
	// is worse than no sentence: it reads as though the evidence exists and the
	// reader failed to find it.
	case !strings.Contains(got, "Three things land here identically"):
		t.Errorf("the report lists no alternative to the image:\n%s\n\nexternalCheck dials "+
			"host+mapped port, so an image that never listened, a starved container and a publish "+
			"path this box did not carry all arrive here identically.", got)
	case strings.Contains(got, "log tail above"):
		t.Errorf("the report points at a log tail it never printed:\n%s\n\nNothing is registered "+
			"in standContainers here, so the lines above say the container was never created. "+
			"Naming evidence that is absent is how a reader loses an hour.", got)
	}

	// The same branch at a SECOND count, which is what makes the assertions above
	// load-bearing. Every number in them derives from 977, so a build that printed
	// the literal 977 instead of reading the cause satisfies all four — checked by
	// mutation, and it survived until this block existed. Two counts is the
	// cheapest thing that cannot be satisfied by any single constant.
	//
	// 700 is still inside this branch: 70s of sleep clears the half-budget floor.
	got = s.describeStandFailure(context.Background(), "postgres",
		fmt.Errorf("postgres container: run postgres: wait until ready: external check: "+
			"check target: retries: 700 address: localhost:34142: get state: %w",
			context.DeadlineExceeded), healthy).Error()
	switch {
	case !strings.Contains(got, "700 container inspections the daemon ANSWERED"):
		t.Errorf("the count in the report does not follow the cause:\n%s\n\nThis cause carries "+
			"700, and a report that prints any other number is reading something that is not "+
			"the wait that failed.", got)
	case !strings.Contains(got, "sleeps come to 1m10s of the 2m0s"):
		t.Errorf("the span does not follow the count:\n%s\n\n700 rounds is 1m10s, not the 1m38s "+
			"the case above produces. A span pinned to one cause is arithmetic about a number "+
			"this report was not given.", got)
	case !strings.Contains(got, "fit in the 50s that is left"):
		t.Errorf("the remainder does not follow the count:\n%s\n\n2m0s less 1m10s is 50s.", got)
	}

	// Known-bad 1: a container that DIED during the same wait, in the envelope the
	// library actually produces for it. checkTarget inspects before every dial and
	// turns a dead container into an error (wait/wait.go:46-57), which externalCheck
	// wraps in the identical "check target: retries: N address:" string — so this
	// arrives with a high count and is one strings.Contains away from being read as
	// a port that never opened. It must not be: the container is dead, and the
	// count says nothing about why.
	exited := errors.New("postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: 312 address: localhost:34142: container exited with code 1")
	got = s.describeStandFailure(context.Background(), "postgres", exited, healthy).Error()
	switch {
	case !strings.Contains(got, "whether the container CHOSE it"):
		t.Errorf("a container that exited is answered without saying what the code cannot "+
			"settle:\n%s\n\nExit 1 inside a readiness wait is either an image that failed or one "+
			"something else stopped, and docker writes both into the same integer. Naming that "+
			"is the verdict; picking one of the two is the defect this ticket is about.", got)
	case strings.Contains(got, "not the thing that was stuck"):
		t.Errorf("a container that exited is described as one that merely never opened its "+
			"port:\n%s\n\nThe count is there and the container is dead; taking the count alone "+
			"turns the one deterministic failure this tier has into a maybe.", got)
	}

	// Known-bad 2: the other format string checkState can return. Two separate
	// strings carry "the container is not running" (wait/wait.go:53 and :55), and a
	// predicate matching only the first drops the second into the port-wait hedge —
	// the same one-of-them miss that made the earlier blocklist wrong.
	//
	// "created" and not "dead": a container that was created and never started is
	// what is left of this family once a remover's statuses are answered by their
	// own branch below. Picking "dead" here would gate that branch instead of this
	// one and leave the residual case untested.
	created := errors.New("redis container: run redis: wait until ready: external check: " +
		`check target: retries: 44 address: localhost:32901: unexpected container status "created"`)
	got = s.describeStandFailure(context.Background(), "redis", created, healthy).Error()
	if !strings.Contains(got, "whether the container CHOSE it") {
		t.Errorf("a container the daemon reports as never started is answered as a port that "+
			"never opened:\n%s\n\n`unexpected container status` and `container exited with code` "+
			"are the same fact in two of the library's format strings, and only one was named.", got)
	}
}

// TestOneWaitEnvelopeIsNotOneVerdict — five inner causes share the string this
// file reads, and they do not share an answer.
//
// This is the guard the first version of the fix did not have. checkTarget wraps
// EVERYTHING it can come back with in the same "check target: retries: N address:
// …" envelope (wait/host_port.go:262-263), so the envelope names the loop and
// says nothing about the layer. The fix then decided from the envelope plus a
// bare count, excluded exited containers by name, and thereby claimed "the daemon
// answered every one of those inspections" over an OOM kill, over a status the
// daemon called dead, and over a docker call that failed outright — three causes
// printed three lines above the sentence denying them. A blocklist fails exactly
// like that: it is a list of the cases somebody thought of.
//
// Each case below is a different inner cause at a count that would have won the
// budget branch, so each one is also a check on the ORDER of the switch: remove
// any branch above it and the count sends it straight into "the daemon was not
// the thing that was stuck".
func TestOneWaitEnvelopeIsNotOneVerdict(t *testing.T) {
	s := &Stack{t: t}
	healthy := daemonVerdict{ping: 8 * time.Millisecond}

	const envelope = "postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: %d address: localhost:34142: %s"
	report := func(cause string, n int, v daemonVerdict) string {
		t.Helper()
		return s.describeStandFailure(context.Background(), "postgres",
			fmt.Errorf(envelope, n, cause), v).Error()
	}

	// The kernel killed it. 812 rounds is 1m21s of sleep, over the bar the budget
	// branch uses, and the string carries no deadline — so this reaches the switch
	// with a high count and a cause that is a fact about the BOX. Reading it as
	// the container also contradicts the container line this same report prints,
	// which spells OOMKilled out as "the machine again".
	//
	// "MACHINE" alone is not enough to assert here, and a mutation proved it: with
	// this branch deleted the cause falls to the one below, which is also MACHINE
	// and says the docker call failed and no container state came back. Both
	// halves of that are false — the inspection SUCCEEDED and reported an OOM
	// kill. Same layer, wrong mechanism, and a reader sent to look at the daemon
	// instead of at how much memory this box has.
	got := report("container crashed with out-of-memory (OOMKilled)", 812, healthy)
	switch {
	case !strings.Contains(got, "read this as the MACHINE"):
		t.Errorf("an OOM kill is not read as the machine:\n%s\n\nwait/wait.go:51 returns this "+
			"before it looks at the exit status, and it means the box ran out of memory. The "+
			"container line in this very report says so; the verdict must not disagree.", got)
	case !strings.Contains(got, "the kernel killed"):
		t.Errorf("an OOM kill is reported as some other machine fault:\n%s\n\nThe layer is right "+
			"and the mechanism is not. checkTarget got its answer here — the container was "+
			"inspected and found killed for memory — so any sentence about a docker call that "+
			"failed sends the reader to the daemon instead of to this box's memory.", got)
	case strings.Contains(got, "no container state at all"):
		t.Errorf("an OOM kill is described as an inspection that came back empty:\n%s\n\nThat "+
			"inspection came back full: it is where the OOM kill was read from.", got)
	case strings.Contains(got, "whether the container CHOSE it"):
		t.Errorf("an OOM kill is filed under exit codes nobody can attribute:\n%s\n\ndocker named "+
			"the mechanism here. This is the one shape in the envelope where who ended the "+
			"container is not in doubt, and hedging it throws that away.", got)
	case strings.Contains(got, "REFUSED"):
		t.Errorf("an OOM kill is reported as refused dials:\n%s\n\nThe wait stopped on an "+
			"inspection, not on a dial; the count is real and the sentence built on it is not.", got)
	}

	// A container killed by a signal, which docker reports as 128+N with no flag
	// and nothing else: `docker kill`, a prune and the OOM killer all arrive here
	// as a bare exit code. Whether a HOST OOM kill ALSO sets OOMKilled is a
	// cgroup-version question this file deliberately does not assert — containerd
	// watches an eventfd on v1 and a counter on v2 — and this branch needs no
	// answer to it: 137 says the process was killed, whoever killed it.
	//
	// Textually that is indistinguishable from an image that exited on its own, and
	// it used to be treated as one: CONTAINER, plus the promise that it would still
	// be here next run. That promise is the whole subject of this ticket.
	got = report("container exited with code 137", 209, healthy)
	switch {
	case strings.Contains(got, "whether the container CHOSE it"):
		t.Errorf("a container that was KILLED is filed under codes nobody can attribute:\n%s\n\n"+
			"137 is 128+9: it died ON SIGKILL, and no service image chooses that. This is one of "+
			"the two codes where the actor IS settled, so hedging it costs the reader a verdict "+
			"the evidence supports.", got)
	case !strings.Contains(got, "read this as the MACHINE"):
		t.Errorf("a container killed from outside the run is blamed on the image:\n%s", got)
	// On "docker reports 137" and not on "137". describeStandFailure echoes the
	// cause verbatim before the switch runs, so the bare number is in the report
	// whatever this branch does with it — an assertion reading the echo survives
	// deleting the exitCode call from the Fprintf entirely. Checked by mutation.
	// One code alone still does not gate it: replacing exitCode(cause) with the
	// constant 137 satisfies this case and only the 143 case below catches it, so
	// the two assertions are load-bearing as a PAIR. Also checked by mutation.
	case !strings.Contains(got, "docker reports 137"):
		t.Errorf("the verdict does not say which code it read:\n%s\n\nThe verdict turns entirely "+
			"on the number, so a reader who disagrees has to see it inside the sentence that "+
			"used it, not only in the cause echoed above.", got)
	case strings.Contains(got, "the kernel killed the container for memory"):
		t.Errorf("a bare SIGKILL is reported as a cgroup OOM kill:\n%s\n\nThat is the "+
			"neighbouring branch's sentence and it is a stronger claim than the evidence: 137 "+
			"says the process was killed, not why. docker kill and a prune produce it too.", got)
	}

	// SIGTERM, the other code docker itself produces. NOT the reaper's doing: a
	// force-remove goes straight to SIGKILL (moby daemon/delete.go → kill.go, no
	// grace period). 143 is what a graceful `docker stop` leaves on an image whose
	// PID 1 does not handle the signal it was sent.
	switch got = report("container exited with code 143", 91, healthy); {
	case strings.Contains(got, "whether the container CHOSE it"):
		t.Errorf("a container stopped by something outside the run is filed under codes nobody "+
			"can attribute:\n%s\n\n143 is 128+15: it died ON SIGTERM rather than returning from "+
			"main, and a process does not choose to do that.", got)
	case !strings.Contains(got, "docker reports 143"):
		t.Errorf("the verdict names a code this cause did not carry:\n%s\n\nThe branch serves two "+
			"codes and the sentence has to carry the one it was given. A branch printing a "+
			"constant reads correctly for the other code and lies about this one.", got)
	}

	// The controls the two cases above need. Without them the split degenerates: a
	// branch reading EVERY exit as a kill satisfies both assertions above and
	// costs the reader the distinction entirely.
	//
	// 130 is the narrow control, and it is inside 128+N — SIGINT. It still does NOT
	// get the kill verdict, and the reason needs no measurement: any program is
	// free to reach 130 by returning it, so the integer cannot separate that from
	// a process that died on SIGINT. Nor is SIGINT hypothetical here —
	// postgres:16-alpine declares STOPSIGNAL SIGINT, so `docker stop` sends it to
	// one of this harness's own stands (what postgres then REPORTS is its own
	// business; docker records the signal it sends AND the integer that comes
	// back, and never which of the two caused the other, so this comment claims
	// only the first). The range this branch owns is therefore
	// {137,143} and not the arithmetic 128+N. Widening the predicate to `>= 128`
	// passes every other assertion here.
	//
	// 0 is the wide control, and the case the old code answered worst. None of
	// these three images has a successful terminal state to reach DURING its own
	// readiness wait, so exit 0 there is a service that was stopped and handled
	// its signal — redis and vault declare no STOPSIGNAL and take the SIGTERM
	// default, which they handle. It used to be called an image that exits on its
	// own, with the promise attached.
	for _, code := range []int{0, 1, 130} {
		got = report(fmt.Sprintf("container exited with code %d", code), 209, healthy)
		switch {
		case !strings.Contains(got, "whether the container CHOSE it"):
			t.Errorf("exit %d is answered without saying what the code cannot settle:\n%s\n\n"+
				"docker writes a program's own exit(2) and a death by signal into one integer, so "+
				"this branch's whole job is to say the code does not decide it and to point at "+
				"the tail that might.", code, got)
		case strings.Contains(got, "read this as the MACHINE"):
			t.Errorf("exit %d is blamed on the box:\n%s\n\n128+N is arithmetic, not evidence. The "+
				"branch above owns the two codes no service image chooses — 137 and 143 — and "+
				"reading a wider range as kills sweeps in exits the program made.", code, got)
		}
	}

	// The reaper's WIDEST window, and the shape the kill branch above does not
	// catch. moby's containerRm sets RemovalInProgress before it kills
	// (daemon/delete.go) and StateString tests that flag ahead of the exited case
	// (container/state.go), so a force-remove is visible as "removing" from the
	// SIGKILL until the 404 — a window strictly wider than the 137 one, and
	// therefore the likelier of the two to be the poll that lands.
	//
	// It arrives through `unexpected container status`, which is the string the
	// categorical verdict used to own outright. So the very actor the kill branch
	// was written for reached this function three ways, and the widest of the
	// three was answered with the promise that the failure was permanent.
	for _, status := range []string{"removing", "dead"} {
		got = report(fmt.Sprintf("unexpected container status %q", status), 44, healthy)
		switch {
		case !strings.Contains(got, "read this as the MACHINE"):
			t.Errorf("a container caught mid-removal is blamed on the image:\n%s\n\nA container "+
				"reaches %q only because something called remove on it, and nothing in this "+
				"harness removes a stand while that stand's own wait is still running.", got, status)
		// The full phrase, not the bare status: the cause is echoed above the
		// switch and already contains `unexpected container status "removing"`, so
		// asserting the word alone passes on a build whose verdict never names it.
		case !strings.Contains(got, fmt.Sprintf("middle of REMOVING (status %q)", status)):
			t.Errorf("the verdict does not say which status it read:\n%s\n\nIt turns on the "+
				"status being one only a remover produces, so the sentence has to carry it.", got)
		case strings.Contains(got, "whether the container CHOSE it"):
			t.Errorf("a container being removed is filed under codes nobody can attribute:\n%s\n\n"+
				"There is no ambiguous exit here — there is no exit at all yet, and the actor is "+
				"named by the status itself.", got)
		}
	}

	// The docker call itself failed — no deadline, no container state, at 640
	// rounds, which is 1m4s of sleep and past the budget branch's bar. This is the
	// case a count-plus-blocklist gets exactly backwards: it is the one shape in
	// this envelope where the daemon really did stop answering, and the branch
	// above it would have said the daemon answered every inspection.
	got = report("get state: Cannot connect to the Docker daemon at unix:///var/run/docker.sock", 640, healthy)
	switch {
	case !strings.Contains(got, "read this as the MACHINE"):
		t.Errorf("an inspection that failed outright is read as the container:\n%s\n\nThe wait "+
			"did not run out of time — a docker call came back with no state at all, which is "+
			"the daemon and nothing else.", got)
	case !strings.Contains(got, "640 inspection(s)"):
		t.Errorf("the report does not say how far into the wait the daemon stopped answering:\n%s",
			got)
	case strings.Contains(got, "ANSWERED"):
		t.Errorf("a wait whose last inspection failed is described as one where every inspection "+
			"was answered:\n%s\n\nThe cause three lines above says the opposite. A count is not a "+
			"licence to describe the round that ended the loop.", got)
	}
	// A second count, because one is satisfied by a constant. Same reason as the
	// budget branch above, and the same mutation restores it here.
	if got = report("get state: Cannot connect to the Docker daemon", 5, healthy); !strings.Contains(
		got, "5 inspection(s)") {
		t.Errorf("the count does not follow the cause:\n%s\n\nThis cause carries 5. A report "+
			"that says anything else is describing a wait it was not given — and how far in the "+
			"daemon went silent is the whole of what this branch adds.", got)
	}

	// The other way an inspection produces no state, and the one that is NOT the
	// daemon failing: it answered, and the answer was a 404. The text is the
	// daemon's body, "No such container: <id>" (moby container/view.go), inside the
	// client's blanket "Error response from daemon: %w" wrap (moby/client
	// request.go:306) — ContainerInspect passes cli.get's error through unchanged
	// in v0.4.0, so BOTH halves are in the real string and the fixture carries
	// both. Without its own branch this lands in the one above, where a container
	// someone else deleted is reported as a daemon falling behind and the reader is
	// sent to a load line measured after the container had already gone.
	got = report("get state: Error response from daemon: No such container: 9f2c1ae40b3d",
		137, healthy)
	switch {
	// The whole clause: the cause is echoed above the switch and carries "No such
	// container" already, so asserting that alone passes on a verdict that never
	// repeats it.
	case !strings.Contains(got, "answered that there is no such container"):
		t.Errorf("a 404 from the daemon is not reported as one:\n%s\n\nThe cause says the "+
			"container does not exist. Any sentence that does not repeat that sends the reader "+
			"to look for a container to inspect.", got)
	case !strings.Contains(got, "something outside this run did"):
		t.Errorf("the report does not say who removed the container:\n%s\n\nNothing in this "+
			"harness removes a container while its own wait is running, and that fact is the "+
			"whole content of the verdict — without it the reader has a 404 and no lead.", got)
	case !strings.Contains(got, "read this as the MACHINE"):
		t.Errorf("a container removed from under the wait is blamed on the image:\n%s", got)
	case strings.Contains(got, "the docker call itself failed"):
		t.Errorf("an answered 404 is described as a docker call that failed:\n%s\n\nThat is the "+
			"neighbouring branch's sentence and it points at daemon load. Load does not remove a "+
			"container mid-wait; a stray reaper or a prune does, and they need different checks.",
			got)
	// 137 is the WAIT COUNT here, not an exit code, and the branch has to print it
	// as one. The pairing with the second call below is what gates it: either
	// assertion alone is satisfied by a report that prints a constant.
	case !strings.Contains(got, "137 inspection(s)"):
		t.Errorf("the report does not say how far into the wait the container disappeared:\n%s\n\n"+
			"When it went is the only ordering evidence a 404 leaves — a container removed at "+
			"round 3 and one removed at round 900 point at different actors.", got)
	}
	if got = report("get state: Error response from daemon: No such container: 9f2c", 8,
		healthy); !strings.Contains(got, "8 inspection(s)") {
		t.Errorf("the count does not follow the cause:\n%s\n\nThis cause carries 8.", got)
	}

	// Both sides of the bar the budget branch draws, as literal counts. 2m0s of
	// budget at 100ms a round is 1200 rounds, so half of it is 600: at 600 the
	// sleeps are most of the window and the daemon had no room to be slow, at 599
	// they are not and it had. Written as numbers rather than as
	// standReadyTimeout/2/waitPollInterval on purpose — a threshold asserted
	// through its own constants moves with them and pins nothing.
	if got := report("get state: context deadline exceeded", 600, healthy); !strings.Contains(
		got, "not the thing that was stuck") {
		t.Errorf("a wait that slept for half its budget is not credited with it:\n%s\n\n600 "+
			"rounds is 1m0s of a 2m0s window; the branch is written >= and this is the value at "+
			"which it must fire.", got)
	}
	// Three counts, and for each one the two spans the sentence divides. The count
	// assertions below gate the numerators and neither denominator, and the spans
	// are the half that carries the verdict: "its sleeps came to X of the budget,
	// so the remaining Y went into the calls" is an argument, not a label. A build
	// that prints a constant `slept` routes to the right branch at all three counts
	// and states the wrong fraction; so does one that swaps the two spans, which
	// prints the arithmetic inverted underneath a verdict the numbers then refute.
	//
	// The spans are literals for the reason the budget branch's are: written as
	// standReadyTimeout-slept they would be true of any budget and pin nothing.
	// And 599 cannot be the only count — it is the one value where the two spans
	// are equal (1m0s and 1m0s), so a swap survives it.
	for _, tc := range []struct {
		n         int
		slept     string // n × 100ms, rounded to the second
		remaining string // what is left of the 2m0s budget after it
	}{
		{n: 599, slept: "1m0s", remaining: "1m0s"},
		{n: 300, slept: "30s", remaining: "1m30s"},
		{n: 10, slept: "1s", remaining: "1m59s"},
	} {
		n := tc.n
		got := report("get state: context deadline exceeded", n, healthy)
		switch {
		case !strings.Contains(got, "read this as the MACHINE"):
			t.Errorf("%d answered inspections inside a spent 2m0s budget are read as a healthy "+
				"daemon:\n%s\n\nThe count is a numerator. %d rounds account for %s of the window; "+
				"the rest of it went into the docker calls, and a call taking that long is the "+
				"machine. Reading the numerator alone is NIM-533's misattribution, restored.",
				n, got, n, (time.Duration(n) * waitPollInterval).Round(time.Second))
		case strings.Contains(got, "ANSWERED"):
			t.Errorf("a wait that spent its budget inside %d docker calls is described as one "+
				"the daemon kept up with:\n%s", n+1, got)
		// The arithmetic, at three counts. The routing assertions above are equally
		// true of a branch that prints a constant instead of reading the cause, and
		// this branch's whole verdict is a division — the count over the budget. A
		// loop is what gates it: no single constant satisfies three values.
		case !strings.Contains(got, fmt.Sprintf("(%d refused dials", n)):
			t.Errorf("the dial count does not follow the cause (%d):\n%s\n\nThe verdict here is "+
				"the count weighed against a spent budget, so a count that is not this wait's "+
				"weighs nothing.", n, got)
		// n+1, not n: the round that failed made its inspection and never reached
		// the dial, so the calls outnumber the refusals by exactly one. Asserting n
		// twice would let the two numbers collapse into one.
		case !strings.Contains(got, fmt.Sprintf("went into the %d docker inspections", n+1)):
			t.Errorf("the inspection count does not follow the dial count (%d vs %d):\n%s\n\n"+
				"checkTarget inspects before every dial and the last round is the one that ran "+
				"out of time, so it inspected once more than it dialled.", n+1, n, got)
		case !strings.Contains(got, fmt.Sprintf("sleeps come to only %s of that", tc.slept)):
			t.Errorf("the sleep span does not follow the count (%d rounds is %s):\n%s\n\nThat "+
				"span is the denominator of this branch's whole argument — small enough that the "+
				"budget went somewhere else, and the somewhere else is the docker calls. A span "+
				"that does not follow the count states a fraction this wait did not have.",
				n, tc.slept, got)
		case !strings.Contains(got, fmt.Sprintf("the remaining %s went into", tc.remaining)):
			t.Errorf("the remainder does not follow the sleeps (2m0s less %s is %s):\n%s\n\nThe "+
				"remainder is what the calls are charged with, and printing the two spans the "+
				"other way round hands the MACHINE verdict a number that refutes it.",
				tc.slept, tc.remaining, got)
		case !strings.Contains(got, "100ms apart)"):
			t.Errorf("the report does not say how far apart the dials were:\n%s\n\nWithout the "+
				"interval the sleep span is an assertion rather than an arithmetic the reader can "+
				"check, and it is the step that turns a count into a duration.", got)
		}
	}

	// A count that is NOT externalCheck's, in both of the library's forms. Three
	// loops print "retries: N" and only one of them opens a socket — the
	// mapped-port loop is waiting for docker to publish a mapping and never dials.
	// An unanchored count reads these too and reports refused dials that never
	// happened, which is a fabricated measurement in the one line of the report
	// that claims to be measured. The second form matters on its own: it contains
	// "check target: retries: 900" verbatim, so an anchor that stopped at "check
	// target:" and dropped " address:" would still pass the first.
	for _, cause := range []string{
		`mapped port: retries: 900, port: "", last err: port not found`,
		`mapped port: check target: retries: 900, port: "", last err: get state: context deadline exceeded`,
	} {
		got := s.describeStandFailure(context.Background(), "postgres",
			errors.New("postgres container: run postgres: wait until ready: "+cause), healthy).Error()
		if strings.Contains(got, "REFUSED") {
			t.Errorf("a wait that never dialled is reported as refused dials:\n%s\n\n"+
				"wait/host_port.go:213-216 counts rounds spent waiting for a port MAPPING; the "+
				"dialer lives only in externalCheck. ` address:` is what tells the two apart, and "+
				"this cause is why the anchor cannot stop at `check target:`.", got)
		}
	}

	// Branch order against the two verdicts that come from outside the cause. The
	// reaper's wait is 60s and hardcoded, so its failure is a statement about a
	// dependency however many rounds are in the string; and a daemon measured as
	// slow stays the machine, because "it answered 977 inspections" is not the
	// opposite of "it was not keeping up". Nothing else in this file passes either
	// verdict together with a full wait envelope, so without these the order is
	// free to drift.
	reaped := fmt.Errorf("postgres container: run postgres: reaper: create container: "+
		"wait until ready: external check: check target: retries: 977 address: localhost:34142: "+
		"get state: %w", context.DeadlineExceeded)
	got = s.describeStandFailure(context.Background(), "postgres", reaped, healthy).Error()
	switch {
	case !strings.Contains(got, "read this as the DEPENDENCY"):
		t.Errorf("a reaper failure carrying a wait envelope is read as the stand's own:\n%s\n\n"+
			"ryuk's 60s wait is hardcoded and nothing here configures it; the count in the string "+
			"belongs to a wait on ryuk's container, not on postgres's.", got)
	case strings.Contains(got, "inspections the daemon ANSWERED"):
		t.Errorf("a reaper failure is handed the count measured by the stand's own wait:\n%s\n\n"+
			"Those 536 dials went to ryuk's address. The line is a measurement of one loop, so it "+
			"belongs inside the one branch that reasons from it — hoisting it above the switch "+
			"decorates every verdict with a number that is about something else.", got)
	}

	loaded := daemonVerdict{ping: daemonWarmPing + time.Second}
	if !loaded.contended() {
		t.Fatalf("a %s ping is not contention, so this case proves nothing about branch order.",
			loaded.ping)
	}
	got = report("get state: context deadline exceeded", 977, loaded)
	switch {
	case !strings.Contains(got, "read this as the MACHINE"):
		t.Errorf("a stand that failed on a daemon too slow to keep up is read as the container:\n%s\n\n"+
			"977 answered inspections do not contradict a saturated daemon — answering is not the "+
			"same as keeping up, and this branch has to stay below the one that measures that.", got)
	case strings.Contains(got, "inspections the daemon ANSWERED"):
		t.Errorf("a MACHINE verdict is prefixed with a line saying the daemon answered everything:\n%s\n\n"+
			"The report would tell the reader the daemon kept up and then tell them it did not, in "+
			"that order. This is the second half of the same defect as the reaper case above: the "+
			"count is only meaningful where it is reasoned from.", got)
	}

	// And the floor: a cause with no wait count at all. Every sentence above is
	// built on a number this one does not have, so the report has to say that
	// rather than reach for the nearest verdict.
	got = s.describeStandFailure(context.Background(), "postgres",
		errors.New("postgres container: run postgres: create container: no such image"), healthy).Error()
	switch {
	case !strings.Contains(got, "nothing in the cause carries the port wait's count"):
		t.Errorf("a failure with no wait count is described as one that has one:\n%s", got)
	case strings.Contains(got, "whether the container CHOSE it"):
		t.Errorf("a cause carrying no exit code at all is answered as an exit code:\n%s\n\nThat "+
			"sentence is about an inspection that found the container stopped. Nothing here "+
			"inspected anything.", got)
	}
}

// TestTheReportNamesALogTailOnlyWhenItPrintedOne — the branch every real reader
// reaches, and the one no fixture in this file used to enter.
//
// The three shapes that land in the budget verdict are separated by the container
// logs, so the report points at them — and in production it can, because
// adoptContainer registers the handle BEFORE the error is checked and a wait that
// failed has a container behind it. Every other test here builds a Stack with an
// empty map, which takes the other arm: "nothing was captured". So the sentence
// that actually ships was covered by nothing, and a mutation that named the tail
// unconditionally would have been caught only by the negative half.
//
// TWO branches now, not one: the budget verdict below and containerGone, which grew
// the same fork in round 4 of this ticket's review. They are guarded together
// because they fail together — both point the reader at a tail to settle a question
// the code declines to settle, and both are reachable with no container handle at
// all, so "the evidence is above" and "there is no evidence" have to be separate
// sentences in each. Three fixtures over six reports: a container with lines, no
// container at all, and — the one that makes this a test about `tailPrinted` and
// not about `ok` — a container that printed nothing.
func TestTheReportNamesALogTailOnlyWhenItPrintedOne(t *testing.T) {
	healthy := daemonVerdict{ping: 8 * time.Millisecond}
	starved := fmt.Errorf("postgres container: run postgres: wait until ready: external check: "+
		"check target: retries: 977 address: localhost:34142: get state: %w", context.DeadlineExceeded)

	withLogs := &Stack{t: t, standContainers: map[string]testcontainers.Container{
		"postgres": logTailStub{logs: "PostgreSQL init process complete; ready for start up.\n" +
			"LOG:  database system is ready to accept connections\n"},
	}}
	got := withLogs.describeStandFailure(context.Background(), "postgres", starved, healthy).Error()
	switch {
	case !strings.Contains(got, "database system is ready to accept connections"):
		t.Errorf("the container printed lines and the report does not show them:\n%s\n\nThey are "+
			"the only input that separates an image which never listened from a box that could "+
			"not carry the port.", got)
	case !strings.Contains(got, "The log tail above separates the first"):
		t.Errorf("the report prints a log tail and never tells the reader what it decides:\n%s\n\n"+
			"A service that already announced itself ready did its part; leaving that inference "+
			"to the reader is the state NIM-469 opened.", got)
	case strings.Contains(got, "Nothing was captured from the container"):
		t.Errorf("the report says nothing was captured, directly under the lines it captured:\n%s",
			got)
	}

	// The other arm, which is what a stand that died before its container had a
	// handle really looks like. Both sentences exist so that neither can be made
	// unconditional, and this is the half that catches it.
	bare := &Stack{t: t}
	got = bare.describeStandFailure(context.Background(), "postgres", starved, healthy).Error()
	if !strings.Contains(got, "Nothing was captured from the container") {
		t.Errorf("a report with no logs does not say so:\n%s\n\nSilently omitting the sentence "+
			"leaves the reader looking for a tail that was never printed.", got)
	}

	// The same fork on containerGone, which did not have one until round 4 of this
	// ticket's review — and which is the branch most likely to be read with no
	// handle at all. `bare` registers nothing, so its container line is the
	// "never created" default; the sibling shape, where the retry deleted the
	// handle it disowned, is a different arm with its own fixture in
	// TestFailureReportDoesNotDenyAContainerTheRetryDeleted. Both of them reach
	// this branch with no tail, which is what made naming one unconditionally
	// wrong: the report said the deciding evidence was above when it was not.
	exited := errors.New("postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: 312 address: localhost:34142: container exited with code 1")
	got = withLogs.describeStandFailure(context.Background(), "postgres", exited, healthy).Error()
	switch {
	case !strings.Contains(got, "The log tail above separates the two readings"):
		t.Errorf("a container that exited with a tail above it is not told what the tail "+
			"decides:\n%s\n\nThe tail is the only input that separates an image which failed "+
			"from one something else stopped, and this is the branch whose whole verdict is "+
			"that it cannot separate them itself.", got)
	case strings.Contains(got, "none was captured"):
		t.Errorf("the report says no tail was captured, directly under the lines it "+
			"captured:\n%s", got)
	}

	got = bare.describeStandFailure(context.Background(), "postgres", exited, healthy).Error()
	switch {
	case strings.Contains(got, "log tail above"):
		t.Errorf("the report points at a log tail it never printed:\n%s\n\nNothing is registered "+
			"in standContainers here, so the lines above say the container was never created. "+
			"This branch used to name the tail unconditionally, which is the sibling defect the "+
			"budget branch was already guarded against.", got)
	case !strings.Contains(got, "none was captured"):
		t.Errorf("a report with no logs does not say so on this branch:\n%s\n\nThe verdict here "+
			"is explicitly that the code cannot settle who ended the container; omitting the one "+
			"sentence that says the deciding evidence is missing leaves it looking settled.", got)
	// And the reading of the rerun it prescribes. Every other branch that says
	// "rerun this test alone" also says what each outcome means; this one said the
	// sentence and stopped, which hands the reader an experiment with no result
	// table — the exact shape this ticket exists to remove.
	case !strings.Contains(got, "failing alone means postgres"):
		t.Errorf("the report orders a rerun and never says how to read it:\n%s\n\nA rerun whose "+
			"outcomes are not named is the state before NIM-533: the harness knows which layer "+
			"each result implicates and leaves the reader to guess.", got)
	}

	// A handle WITHOUT a tail, which is the fixture the four above cannot supply
	// and the one that makes this test about `tailPrinted` rather than about `ok`.
	//
	// tailPrinted is set in exactly one place — inside `case ok:`, and only when
	// containerLogTail came back non-empty (daemonhealth.go:641-649) — so it
	// implies `ok` and the two differ on precisely one input: a registered
	// container that printed nothing. Every fixture above is either both or
	// neither, so `if ok {` reads identically to `if tailPrinted {` on all of
	// them, compiles, and keeps this test green while both branches point at a
	// tail that is not there. So does hoisting `tailPrinted = true` out of the
	// `tail != ""` check. This fixture is what makes those two red.
	//
	// It is not a contrivance either: containerLogTail returns "" when Logs
	// errors, when the read yields nothing, and when the container genuinely
	// printed nothing before it died (daemonhealth.go:1178-1206) — a daemon
	// refusing the log stream and a container killed before its first line are
	// the very failures these branches are written about.
	mute := &Stack{t: t, standContainers: map[string]testcontainers.Container{
		"postgres": logTailStub{},
	}}
	for _, tc := range []struct {
		branch  string
		cause   error
		missing string // the fragment both branches print when they HAVE a tail
		says    string // what THIS one must print instead when it does not
	}{
		{branch: "containerGone", cause: exited,
			missing: "log tail above", says: "none was captured"},
		{branch: "waitOutlivedItsBudget", cause: starved,
			missing: "log tail above", says: "Nothing was captured from the container"},
	} {
		got = mute.describeStandFailure(context.Background(), "postgres", tc.cause, healthy).Error()
		switch {
		case strings.Contains(got, "last lines it printed"):
			// Checked on the OUTPUT, not on tailPrinted, because the two can be split
			// the other way round: hoist the Fprintf out of the emptiness check and
			// leave the flag inside, and the fork below still says nothing was
			// captured while the report has already opened an empty evidence block
			// above it. The flag stays honest; only the text stops being.
			t.Errorf("the %s report opens a log-tail block for a container that printed "+
				"nothing:\n%s\n\nThe block has nothing under it, so a reader who trusts the "+
				"heading goes looking for lines that were never there. What the sentence beneath "+
				"it says depends on which way the flag was split from the print — read the report "+
				"above rather than assuming.", tc.branch, got)
		case strings.Contains(got, tc.missing):
			t.Errorf("the %s branch points at a log tail for a container that printed "+
				"nothing:\n%s\n\nThe handle exists, so the report can describe the container — but "+
				"no lines were captured from it, and this is the one input on which `ok` and "+
				"`tailPrinted` differ. A fork written on the former says the evidence is above "+
				"whenever a handle was registered, which is not what it means.", tc.branch, got)
		case !strings.Contains(got, tc.says):
			t.Errorf("the %s branch does not say the tail is missing:\n%s\n\nIt printed the "+
				"container line and then nothing about the logs, which reads as a report that "+
				"simply had no more to add rather than one whose deciding evidence is absent.",
				tc.branch, got)
		}
	}
}

// shapeTableRow — one "unexpected container status" row of the header table in
// daemonhealth.go: the status it names, and the verdict column beside it.
var shapeTableRow = regexp.MustCompile(`unexpected container status "([a-z]+)"(.*)`)

// TestTheShapeTableSplitsTheStatusesTheWayThePredicatesDo — the header table is
// read as a partition of the library's shapes, so it has to be one.
//
// That block ends "The predicates below name them one at a time", which is an
// instruction to whoever writes the next predicate: check it against this list.
// It is therefore load-bearing prose — and it was wrong for a whole round of
// this ticket. Its residual row named four statuses, of which two cannot occur
// and one belonged to the row above it. Nothing went red, because nothing read
// it, and a table nothing reads is how the next blocklist gets written.
//
// Two checks, and they come from different places on purpose. WHICH statuses can
// arrive is moby's answer, not this package's, so it is written down here with
// its citation rather than derived. WHICH row each belongs on is this package's
// answer, and it is taken from beingRemoved instead of from the prose — that is
// the half that catches a status drifting between a predicate and the table that
// documents it.
func TestTheShapeTableSplitsTheStatusesTheWayThePredicatesDo(t *testing.T) {
	src, err := os.ReadFile("daemonhealth.go")
	if err != nil {
		t.Fatalf("read daemonhealth.go: %v", err)
	}
	const opening = "// One envelope,"
	start := strings.Index(string(src), opening)
	if start < 0 {
		t.Fatalf("the shape table is gone from daemonhealth.go (no %q). It is the list every "+
			"new predicate gets checked against; losing it silently costs more than the table.",
			opening)
	}
	var table strings.Builder
	for _, line := range strings.Split(string(src)[start:], "\n") {
		if !strings.HasPrefix(line, "//") {
			break
		}
		table.WriteString(line + "\n")
	}

	// Each row, and whether its own verdict column says a remover did it.
	rows := map[string]bool{}
	for _, m := range shapeTableRow.FindAllStringSubmatch(table.String(), -1) {
		rows[m[1]] = strings.Contains(strings.ToLower(m[2]), "remov")
	}

	// moby container/state.go at v28.5.2: StateString returns "paused" and
	// "restarting" only from inside `if s.Running`, and checkState answers
	// state.Running before it ever looks at the status (wait/wait.go:48), so
	// neither can reach the default arm. "exited" leaves through its own arm one
	// line earlier. What StateString can still return with Running false is these
	// three, and nothing else.
	for _, status := range []string{"removing", "dead", "created"} {
		if _, ok := rows[status]; !ok {
			t.Errorf("the shape table does not name %q, which checkState can hand this file:\n%s\n\n"+
				"A shape missing from the table is a shape the next predicate is not checked "+
				"against — which is exactly how the branch this ticket fixed was written.",
				status, table.String())
		}
	}
	for status := range rows {
		switch status {
		case "removing", "dead", "created":
		default:
			t.Errorf("the shape table names %q, which does not reach the default arm:\n%s\n\n"+
				"Only three statuses do. \"paused\" and \"restarting\" are returned by "+
				"StateString solely with Running true and checkState answers Running first; "+
				"\"exited\" leaves through its own arm one line earlier and carries a code, which "+
				"is the whole reason it is read differently. Whichever of those this row is, a "+
				"reader partitioning predicates against it is guarding a case that does not "+
				"arrive, and will conclude the real residue is already covered.",
				status, table.String())
		}
	}

	// And the split itself, taken from the predicate rather than from the prose.
	for status, saysRemover := range rows {
		cause := fmt.Errorf("redis container: run redis: wait until ready: external check: "+
			"check target: retries: 44 address: localhost:32901: unexpected container status %q",
			status)
		if got := beingRemoved(cause); got != saysRemover {
			t.Errorf("the table and beingRemoved disagree about %q — the table's verdict column "+
				"names a removal: %v, the predicate answers it: %v:\n%s\n\nOne of the two is "+
				"what the next reader will trust and they cannot both be it. \"dead\" is the "+
				"live example: cleanupContainer sets Dead after the stop and before it releases "+
				"the layer and the root, so a force-remove failing in either leaves it (moby "+
				"daemon/delete.go), which puts it with the remover and not with the residue.",
				status, saysRemover, got, table.String())
		}
	}
}

// TestNoBranchPromisesTheRedWillSurviveARerun — the ticket's invariant, stated
// once over every branch instead of once per case.
//
// NIM-646 began as one branch making that promise. Fixing it case by case is how
// the same defect keeps coming back one cause at a time: the kill branch was
// added for a stray reaper and the same reaper still reached the categorical
// through "removing", because a fix aimed at a string is not aimed at the claim.
//
// The claim is that nothing this function reads can establish that a red will
// reproduce. The log tail could, and it is not parsed; every other input is a
// count, a status or an exit code, and none of them separates an image that
// failed from a container something else ended. So no branch may say it — and
// this test is the one place that has to be updated if a branch ever earns it.
//
// It gates the literal sentence the code used to print. That is deliberate and
// it is the limit: a paraphrase would pass. The mutation battery for this ticket
// restores this exact string into four different branches, which is the shape
// the regression actually takes.
func TestNoBranchPromisesTheRedWillSurviveARerun(t *testing.T) {
	s := &Stack{t: t}
	healthy := daemonVerdict{ping: 8 * time.Millisecond}
	const wrapped = "postgres container: run postgres: wait until ready: external check: " +
		"check target: retries: %d address: localhost:34142: %s"

	// One cause per branch of the switch, so a promise reintroduced anywhere is
	// caught here whether or not that branch has a case of its own elsewhere.
	for _, tc := range []struct {
		branch string
		inner  string
		count  int
	}{
		{"waitOutlivedItsBudget", "get state: context deadline exceeded", 977},
		{"inPortWait", "get state: context deadline exceeded", 12},
		{"outOfMemory", "container crashed with out-of-memory (OOMKilled)", 88},
		{"killedBySignal/137", "container exited with code 137", 209},
		{"killedBySignal/143", "container exited with code 143", 91},
		{"beingRemoved/removing", `unexpected container status "removing"`, 44},
		{"beingRemoved/dead", `unexpected container status "dead"`, 44},
		{"containerGone/0", "container exited with code 0", 209},
		{"containerGone/1", "container exited with code 1", 312},
		{"containerGone/created", `unexpected container status "created"`, 44},
		{"containerVanished", "get state: Error response from daemon: No such container: 9f2c", 137},
		{"daemonStoppedAnswering", "get state: Cannot connect to the Docker daemon", 640},
	} {
		cause := fmt.Errorf(wrapped, tc.count, tc.inner)
		got := s.describeStandFailure(context.Background(), "postgres", cause, healthy).Error()
		if strings.Contains(got, "does not go away on a rerun") {
			t.Errorf("%s promises the failure will reproduce:\n%s\n\nNothing in this cause "+
				"establishes that. The three live runs that opened this ticket printed that "+
				"sentence and the stands it condemned passed alone in 9.5-58s.", tc.branch, got)
		}
	}

	// The floor, and the three branches the loop above cannot reach: a cause with
	// no wait count, the reaper's own failure, and the contended arm — that last
	// one is entered by the VERDICT, which is checked ahead of every cause, so no
	// row above can reach it however its cause is written. Sweeping only causes
	// left it uncovered; the mutation battery restores the sentence there too.
	for _, tc := range []struct {
		cause error
		v     daemonVerdict
	}{
		{errors.New("postgres container: run postgres: create container: no such image"), healthy},
		{fmt.Errorf("reaper: %w", errors.New("wait until ready: external check: check target: "+
			"retries: 536 address: localhost:33791: dial tcp: connect: connection refused")), healthy},
		{fmt.Errorf(wrapped, 12, "get state: context deadline exceeded"),
			daemonVerdict{ping: 3 * time.Second}},
	} {
		got := s.describeStandFailure(context.Background(), "postgres", tc.cause, tc.v).Error()
		if strings.Contains(got, "does not go away on a rerun") {
			t.Errorf("a branch outside the cause sweep promises the failure will reproduce:\n%s", got)
		}
	}
}

// logTailStub answers the two calls describeStandFailure makes of a container
// handle and nothing else.
//
// The embedded interface is nil on purpose: anything this report starts calling
// beyond State and Logs panics here rather than being quietly satisfied, which is
// the difference between a stub and a fixture that hides new behaviour. State
// reports a RUNNING container, because that is the shape this test is about — the
// process is up and the published address is still refusing.
type logTailStub struct {
	testcontainers.Container
	logs string
}

func (c logTailStub) State(context.Context) (*container.State, error) {
	return &container.State{Status: "running", Running: true}, nil
}

func (c logTailStub) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(c.logs)), nil
}

// TestRefusalGuidanceSeparatesSilenceFromAnAnswer — a daemon that answered with
// an error must not be described with the prose written for one that said
// nothing.
//
// bringUpStand refuses on unresponsive(), which is the union of the two: both
// mean the stand cannot be attempted. But they are different facts about the
// machine and they have different fixes. Silence is the WSL2 relay shape and
// costs the full probe budget to establish; an error is a setup fact — no
// socket, wrong DOCKER_HOST, no permission on it — and arrives in milliseconds.
// Sending someone to rebuild WSL Integration over a typo in DOCKER_HOST is a
// worse outcome than the raw library error this file replaced.
func TestRefusalGuidanceSeparatesSilenceFromAnAnswer(t *testing.T) {
	silent := daemonVerdict{ping: daemonProbeBudget, err: errDaemonUnresponsive}
	answered := daemonVerdict{ping: 3 * time.Millisecond, err: errors.New("permission denied while trying to connect to the docker daemon socket")}

	if !silent.silent() {
		t.Fatalf("a verdict carrying errDaemonUnresponsive is not reported as silence; the two "+
			"halves of unresponsive() have collapsed and every refusal now reads the same. got %v",
			silent)
	}
	if answered.silent() {
		t.Fatalf("a fast error from the daemon is reported as silence. It answered — in %s — and "+
			"the report will tell the reader to rebuild WSL Integration over what is a setup "+
			"mistake. got %v", answered.ping, answered)
	}

	const wsl = "WSL Integration"
	if g := silent.refusalGuidance("postgres"); !strings.Contains(g, wsl) {
		t.Errorf("the guidance for a silent daemon no longer names the shape it is: %q", g)
	}
	got := answered.refusalGuidance("postgres")
	if strings.Contains(got, wsl) {
		t.Errorf("the guidance for a daemon that ANSWERED sends the reader to rebuild WSL "+
			"Integration. The socket was reachable and refused the call; the fix is DOCKER_HOST "+
			"or a permission, and this text will cost someone an afternoon. got %q", got)
	}
	if !strings.Contains(got, "DOCKER_HOST") {
		t.Errorf("the guidance for a daemon that answered with an error does not name what to "+
			"check. An error in milliseconds is a setup fact and the report has to say so: %q", got)
	}
	if s := silent.String(); !strings.Contains(s, "did not answer within") {
		t.Errorf("silence is no longer described as silence: %q", s)
	}
	if s := answered.String(); strings.Contains(s, "did not answer within") {
		t.Errorf("a %s refusal is described as %q — a wait that never happened, stated as a "+
			"measurement: %q", answered.ping, "did not answer within "+daemonProbeBudget.String(), s)
	}
}

// TestFailureReportDoesNotDenyAContainerTheRetryDeleted — "never created" is a
// claim, and after a retry it is usually a false one.
//
// terminateStandAttempt tears the failed attempt down and drops it from
// standContainers, which is correct — the retry must not inherit a corpse. But
// if the second attempt then dies before a handle exists, the map is empty and
// the report would say the image never ran, about an image that ran, failed, and
// had the logs explaining it deleted by this harness. That sends the reader
// looking for an image-pull failure that never happened.
func TestFailureReportDoesNotDenyAContainerTheRetryDeleted(t *testing.T) {
	s := &Stack{standRetriedAway: map[string]bool{"postgres": true}}
	msg := s.describeStandFailure(context.Background(), "postgres",
		errors.New("create container: context deadline exceeded"), daemonVerdict{ping: 5 * time.Millisecond}).Error()

	if strings.Contains(msg, "never created") {
		t.Errorf("the report says the container was never created, but this harness terminated an "+
			"earlier attempt of it. The logs that would explain the failure were deleted by the "+
			"retry, and the report denies they ever existed: %q", msg)
	}
	if !strings.Contains(msg, "torn down by the retry") {
		t.Errorf("the report does not say the retry is what removed the evidence. A reader who is "+
			"not told this cannot know why there are no logs: %q", msg)
	}

	// Negative control: with no retry recorded, "never created" is the true
	// statement and has to survive — otherwise this guard would pass on a
	// version that simply deleted the branch.
	fresh := &Stack{}
	if m := fresh.describeStandFailure(context.Background(), "redis",
		errors.New("create container: no such image"), daemonVerdict{ping: 5 * time.Millisecond}).Error(); !strings.Contains(m, "never created") {
		t.Errorf("a stand that really never got a container no longer says so: %q", m)
	}

	// And the ORDER of the two arms, which decides the ordinary retry rather than
	// a corner of it. A stand whose first attempt registered a container and then
	// failed in a way that bought a retry arrives here with BOTH records set: that
	// attempt was disowned, which is what writes the map, and attempt two adopted its own
	// container before its error was checked — an invariant with its own guard,
	// TestRaisedContainersAreAdoptedBeforeTheErrorCheck, because testcontainers
	// hands back a live container alongside a failed wait.
	//
	// Both qualifiers are load-bearing rather than hedging. disownContainer
	// returns without writing anything when there is no handle to erase, so a
	// first attempt that died before a container existed leaves only the second
	// attempt's record — reachable, because v.contended() buys a retry whatever
	// the cause says. And a cause that buys no retry ends the stand on attempt
	// one, with no disown to record at all. What is left after both exclusions is
	// still the ordinary retry rather than a corner of it: the two witnesses that
	// read the cause instead of the daemon both need the port wait's counter, and
	// a wait that counted is a wait a container reached.
	//
	// So the arms are not disjoint, and putting the narrower-looking one first —
	// the reorder a maintainer makes reasoning that a specific case belongs above
	// a general one — compiles, keeps every fixture above green, and prints "the
	// logs that would explain this are gone" while the report is holding a handle
	// whose logs it could read. Neither map ALONE separates the two orders; this
	// is the input that does.
	both := &Stack{
		t:                t,
		standContainers:  map[string]testcontainers.Container{"postgres": logTailStub{logs: "FATAL: could not create shared memory segment\n"}},
		standRetriedAway: map[string]bool{"postgres": true},
	}
	held := both.describeStandFailure(context.Background(), "postgres",
		errors.New("create container: context deadline exceeded"), daemonVerdict{ping: 5 * time.Millisecond}).Error()
	if strings.Contains(held, "the logs that would explain this are gone") {
		t.Errorf("the report calls its evidence deleted while it is holding the handle:\n%s\n\n"+
			"An earlier attempt WAS torn down and the record saying so is right, but this attempt "+
			"registered a container of its own before it failed and those logs are readable. The "+
			"handle has to be answered first — the disown record speaks only for the case where "+
			"nothing replaced it.", held)
	}
	if !strings.Contains(held, "could not create shared memory segment") {
		t.Errorf("the report does not print the lines it could read:\n%s\n\nA live handle after a "+
			"retry still has a log tail, and it is the only account of why THIS attempt failed.",
			held)
	}
}

// TestDisowningAContainerRecordsThatItExisted — the producer half of the guard
// above.
//
// Written after the consumer half passed a mutation that deleted the recording
// outright: a test that hands describeStandFailure a pre-set map proves what the
// report does with the record and nothing at all about whether anything ever
// writes it. Two claims, two known-bads — the NIM-469 lesson, missed once more
// here on the first attempt.
func TestDisowningAContainerRecordsThatItExisted(t *testing.T) {
	// A typed nil is a legitimate non-nil interface value, and disownContainer
	// only ever compares handles — it never calls through one. That keeps this a
	// test of the bookkeeping rather than of a container double.
	var stub testcontainers.Container = (*testcontainers.DockerContainer)(nil)
	s := &Stack{standContainers: map[string]testcontainers.Container{"postgres": stub}}

	s.disownContainer("postgres")

	if _, still := s.standContainers["postgres"]; still {
		t.Fatalf("disownContainer left the handle in the map; the retry's second attempt would " +
			"inherit the first attempt's corpse")
	}
	if !s.standRetriedAway["postgres"] {
		t.Errorf("disownContainer erased the only evidence that this stand ever had a container " +
			"and recorded nothing. describeStandFailure will then report 'never created' about " +
			"an image that ran, failed, and had its logs deleted by this harness.")
	}

	// Negative control: a stand nobody disowned must not be marked, or the
	// report would claim a retry that never happened.
	fresh := &Stack{}
	fresh.disownContainer("redis")
	if fresh.standRetriedAway["redis"] {
		t.Errorf("disownContainer marked a stand it never held a container for; the failure " +
			"report would blame a retry that did not happen for missing logs")
	}
}

// harnessVarFuncLit returns the function literal a package-level var is
// initialised with, or fails. Vars rather than funcs: the seams these guards
// check are variables precisely so a test can substitute them.
func harnessVarFuncLit(t *testing.T, name string) *ast.FuncLit {
	t.Helper()

	var found *ast.FuncLit
	forEachHarnessFile(t, func(_ string, file *ast.File, _ *token.FileSet) {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if id.Name != name || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.FuncLit); ok {
						found = lit
					}
				}
			}
		}
	})
	if found == nil {
		t.Fatalf("no package-level var %s initialised with a function literal in the harness "+
			"sources. This guard's subject is that literal; if it moved, the guard is checking "+
			"nothing.", name)
	}
	return found
}

// harnessFuncNamed returns a top-level function from the harness's non-test
// sources, or fails. Failing rather than returning nil on purpose: every caller
// here is a guard whose subject is that function, and a guard that cannot find
// its subject has to say so instead of passing.
func harnessFuncNamed(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()

	var found *ast.FuncDecl
	forEachHarnessFile(t, func(_ string, file *ast.File, _ *token.FileSet) {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
				found = fn
			}
		}
	})
	if found == nil {
		t.Fatalf("no function named %s in the harness sources. If it was renamed, this guard is "+
			"now checking nothing and the rename has to bring it along.", name)
	}
	return found
}

// callsMethod / callsMethodOn / exprCallsMethod — the small AST predicates the
// guards above share. Method calls only, deliberately: probe_test.go's callsIdent
// matches a bare identifier too, and here that would let a local helper named
// Start satisfy "this function starts a process".
func callsMethod(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && exprCallsMethod(call, name) {
			found = true
		}
		return true
	})
	return found
}

func callsMethodOn(n ast.Node, recv, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
			found = true
		}
		return true
	})
	return found
}

func exprCallsMethod(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

// TestMissingKeeperBinaryFailsTheTierInsteadOfSkipping — a stand that cannot be
// built reports a FAILURE, and reports it as bring-up.
//
// NIM-533. The pre-flight skipped until this ticket, and that skip was how L3a
// certified work it never did. `go test` without `-v` prints `ok <pkg> 0.1s` for
// a package whose tests all skipped — byte-for-byte what it prints when they all
// passed. So a run that located no keeper binary exited 0, satisfied `make e2e`
// and every gate above it, and never reached scripts/classify-l3a-failure.py,
// which the Makefile invokes only on a non-zero status: the tool's own NOT-RUN
// verdict was unreachable from its only caller. Nothing in the log said so,
// because there was nothing in the log. That is the same disease as the red this
// batch is about — a result indistinguishable from the harness hiccupping — at
// the opposite pole, and the more dangerous pole, because nobody investigates a
// green run.
//
// Graded in a CHILD PROCESS, on the exit status and the reported outcome,
// because that is the level the defect lived at. A skip inside this process is
// invisible here — t.Skip in a subtest leaves t.Run returning true, exactly the
// blindness being fixed — and asserting an internal branch instead would
// re-state the code rather than what `go test` tells a gate.
func TestMissingKeeperBinaryFailsTheTierInsteadOfSkipping(t *testing.T) {
	const childEnv = "L3A_PREFLIGHT_CHILD"

	if os.Getenv(childEnv) == "1" {
		// The case itself. KEEPER_BIN points at nothing, so the pre-flight is
		// what decides, and whatever it does is what the parent grades.
		NewStack(t, Config{})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=90s")
	cmd.Env = append(os.Environ(),
		childEnv+"=1",
		"KEEPER_BIN="+filepath.Join(t.TempDir(), "keeper-that-was-never-built"),
	)
	out, err := cmd.CombinedOutput()
	got := string(out)

	if err == nil {
		t.Errorf("a stand with no keeper binary let `go test` exit 0. An L3a run that "+
			"builds no stand is then reported as a pass, and `make e2e` never calls the "+
			"classifier:\n%s", got)
	}
	if strings.Contains(got, "--- SKIP:") {
		t.Errorf("the pre-flight SKIPPED. A package of skipped tests prints `ok <pkg>` and "+
			"exits 0, so this is the tier saying it passed while it ran nothing:\n%s", got)
	}
	if !strings.Contains(got, "--- FAIL:") {
		t.Errorf("no `--- FAIL:` in the child. Whatever ended it, it was not the pre-flight "+
			"reporting a missing input the way this tier reports every other one:\n%s", got)
	}
	// Necessary on top of the failure, not implied by it. Read from the constant
	// rather than copied, so a rewording moves both together.
	if !strings.Contains(got, standSetupMarker) {
		t.Errorf("the refusal is not declared as bring-up: no %q in the child's output.\n"+
			"A missing binary is the machine, not a finding about the code, and undeclared it "+
			"arrives as a bare `--- FAIL: TestX` on all forty tests at once — the shape this "+
			"tier is least able to read. The declaration is a defer, so registering it BELOW "+
			"the pre-flight silently drops this case while every other guard stays "+
			"green:\n%s", standSetupMarker, got)
	}
}

// TestNoHarnessEntryPointSkipsOnAMissingEnvironment — the harness never answers
// "I cannot run" with a skip.
//
// The guard above proves the two entry points that exist today fail. This one
// holds for the entry point added next year, and it is a separate claim: the
// mechanism is not "NewStack is correct" but "`ok <pkg>` means tests ran", and
// one t.Skipf anywhere in these sources is a licence to report a pass for a
// stand that was never built.
//
// Scoped to the harness sources, not the tests: a deliberately parked TEST still
// skips (redis_test.go and oracle_typed_portent_test.go do, each naming the
// batch that unparks it). That is a decision someone made and a reader can find
// in the diff. The harness discovering it has no keeper binary is not a
// decision, it is a failure, and a skip is this tier's one way to render a
// failure invisible.
func TestNoHarnessEntryPointSkipsOnAMissingEnvironment(t *testing.T) {
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Skip/Skipf/SkipNow end the test; Skipped() only reports. Naming
			// the three rather than prefix-matching keeps the query out.
			switch sel.Sel.Name {
			case "Skip", "Skipf", "SkipNow":
			default:
				return true
			}
			t.Errorf("%s:%d: the harness calls %s. A package whose tests all skipped prints "+
				"`ok <pkg> 0.1s` and exits 0 — indistinguishable from one where they all "+
				"passed — so this reports a pass for a stand that never came up, and "+
				"scripts/classify-l3a-failure.py is never reached to say otherwise. If the "+
				"environment is missing something this tier needs, fail: the caller asked for "+
				"L3a by building with -tags=e2e.",
				base, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			return true
		})
	})
}
