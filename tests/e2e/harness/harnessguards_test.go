//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
	reaperErr := fmt.Errorf("reaper: %w", fmt.Errorf("new reaper: %w",
		errors.New("container did not become ready: get state: context deadline exceeded: retries: 536")))

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
