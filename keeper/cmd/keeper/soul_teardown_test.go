package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

// [soulTeardown] is the ONLY real implementation of soulforget.Teardown — the
// unit tests of the forget path all drive a scripted fake, which proves that
// Erase RESPECTS a failed broadcast but proves nothing about whether the real
// broadcast can fail, or whether the real purge deletes anything. This file
// tests the seam itself: without it, `return 3, nil` from PurgeCache and
// `return true, nil` from Broadcast are both green on every tier, and each is
// literally "the record was deleted and the resource was not released".
//
// Everything here is exercised through the production Redis helpers over
// miniredis, so a renamed key or a changed publish path breaks both ends at
// once instead of leaving a green test pointing at nothing.

// TestSoulTeardown_IsWiredIntoBothOperatorSurfaces — everything below proves
// the adapter works. This proves anything reaches it.
//
// `api.Deps.SoulTeardown` and `mcp.HandlerDeps.SoulTeardown` are plain nil-able
// interface fields, and [soulforget.Erase] reads a nil Teardown as "no cluster,
// nothing to release" and deletes anyway — correct for unit tests, catastrophic
// in `keeper run`. So deleting either wiring line compiles, keeps every test in
// this repo green, and turns the endpoint into a bare row delete: the operator
// is handed a 200 with local_stream_closed=false, broadcast=false,
// cache_keys_purged=0 and NO warnings, while the forgotten host's EventStream
// stays open on this very instance and its TTL-less `soul:<sid>:hb` leaks for
// good. Deleted the record, released nothing, reported success.
//
// Neither setup function can be called from a test — both build a live server
// over a real pool — so the guard reads the wiring out of the source, the way
// credential_out_test.go guards the stdout rule.
func TestSoulTeardown_IsWiredIntoBothOperatorSurfaces(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}

	for _, fnName := range []string{"setupAPIServer", "setupMCPServer"} {
		fn := findDaemonMethod(file, fnName)
		if fn == nil {
			t.Fatalf("method (*daemon).%s not found in daemon.go — this guard now guards nothing", fnName)
		}

		var wiredTo string
		ast.Inspect(fn, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "SoulTeardown" {
				return true
			}
			if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "nil" {
				t.Errorf("%s sets SoulTeardown to nil at %s — forgetting a host would erase its "+
					"registry row and release none of what it holds", fnName, fset.Position(kv.Pos()))
				return true
			}
			wiredTo = teardownValueName(kv.Value)
			return true
		})

		switch wiredTo {
		case "":
			t.Errorf("%s builds its deps without SoulTeardown — `DELETE /v1/souls/{sid}` and "+
				"`keeper.soul.forget` would delete the row while the host's EventStream stays "+
				"open here and `soul:<sid>:hb` is left behind with no TTL, and the reply would "+
				"say so only by omission: no warnings, everything released reported as false/0",
				fnName)
		case soulTeardownTypeName:
			// The production adapter, whose behaviour the rest of this file
			// exercises over miniredis. Pinning the NAME is what joins the two
			// halves: without it this guard proves the field is populated, and
			// the miniredis tests prove soulTeardown works, and nothing at all
			// proves the populated value IS soulTeardown.
		default:
			t.Errorf("%s wires SoulTeardown to %s, not %s — non-nil is not the invariant. A type "+
				"that satisfies the interface and releases nothing (`return true, nil` from "+
				"Broadcast, `return 0, nil` from PurgeCache) passes a non-nil check and hands the "+
				"operator a 200 for a host whose stream is still open here",
				fnName, wiredTo, soulTeardownTypeName)
		}
	}
}

// soulTeardownTypeName — the production soulforget.Teardown adapter in this
// package. If it is renamed, this guard fails and points at itself; update the
// constant, do not relax the check.
const soulTeardownTypeName = "soulTeardown"

// teardownValueName reduces the expression assigned to SoulTeardown to the name
// a reader would call it by: the type of a composite literal, or the function of
// a constructor call. Returns "" for a shape it cannot name, which the caller
// reports as "not wired" — an unreadable wiring is not a wiring this guard can
// vouch for.
func teardownValueName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.CompositeLit:
		return exprName(v.Type)
	case *ast.UnaryExpr: // &soulTeardown{…}
		return teardownValueName(v.X)
	case *ast.CallExpr:
		return exprName(v.Fun)
	case *ast.Ident:
		return v.Name
	default:
		return ""
	}
}

func exprName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return exprName(v.X)
	default:
		return ""
	}
}

// findDaemonMethod is findFuncDecl (credential_out_test.go) for methods — the
// setup functions hang off *daemon, so they have a receiver.
func findDaemonMethod(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// newTeardownDaemon builds the minimum daemon the adapter reads: a stream
// manager, a Redis client (nil when redis is false) and a KID.
func newTeardownDaemon(t *testing.T, redis bool) (soulTeardown, *miniredis.Miniredis) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

	d := &daemon{
		logger:        logger,
		cfg:           &config.KeeperConfig{KID: "keeper-test-01"},
		streamManager: keepergrpc.NewStreamManager(logger),
	}
	var mr *miniredis.Miniredis
	if redis {
		mr = miniredis.RunT(t)
		c, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: mr.Addr()}, nil)
		if err != nil {
			t.Fatalf("redis.NewClient: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		d.redisClient = c
	}
	return soulTeardown{d: d}, mr
}

// TestSoulTeardown_PurgeCache_ActuallyDeletesTheKeys — the count in the
// operator's reply must come from a real DEL. A fabricated number here is the
// exact defect the ticket names: the row is gone, `soul:<sid>:hb` has no TTL,
// nothing will ever collect it, and the operator was told it was released.
func TestSoulTeardown_PurgeCache_ActuallyDeletesTheKeys(t *testing.T) {
	td, mr := newTeardownDaemon(t, true)
	ctx := context.Background()
	const sid = "host1.example.com"

	if err := keeperredis.TouchHeartbeat(ctx, td.d.redisClient, sid, "kid-1", time.Now()); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	if !mr.Exists(keeperredis.HeartbeatKey(sid)) {
		t.Fatal("precondition: the heartbeat key was never written, the test would prove nothing")
	}

	n, err := td.PurgeCache(ctx, sid)
	if err != nil {
		t.Fatalf("PurgeCache: %v", err)
	}
	if n < 1 {
		t.Errorf("purged = %d, want at least the heartbeat key", n)
	}
	if mr.Exists(keeperredis.HeartbeatKey(sid)) {
		t.Errorf("PurgeCache returned %d but %q is still there — the reply counts keys "+
			"it did not delete", n, keeperredis.HeartbeatKey(sid))
	}
}

// TestSoulTeardown_PurgeCache_WithoutRedisPurgesNothingAndSaysSo — a Keeper
// without Redis has no per-SID cache, so there is nothing to release and no
// failure either. Reporting a count would be a release that never happened.
func TestSoulTeardown_PurgeCache_WithoutRedisPurgesNothingAndSaysSo(t *testing.T) {
	td, _ := newTeardownDaemon(t, false)

	n, err := td.PurgeCache(context.Background(), "host1.example.com")
	if err != nil {
		t.Fatalf("PurgeCache without redis: %v, want no error (single-instance is not a failure)", err)
	}
	if n != 0 {
		t.Errorf("purged = %d without a Redis client, want 0", n)
	}
}

// TestSoulTeardown_Broadcast_ReachesTheOtherInstances — the notice has to leave
// this process. A `return true, nil` stub is green everywhere else and silently
// removes the pre-flight in soulforget.Erase along with it: if Broadcast cannot
// fail, ErrTeardownUnavailable is unreachable and "nothing was deleted when the
// cluster is unreachable" stops being a property.
func TestSoulTeardown_Broadcast_ReachesTheOtherInstances(t *testing.T) {
	td, _ := newTeardownDaemon(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peer, err := keeperredis.SubscribeSoulForget(ctx, td.d.redisClient, "keeper-peer-02", td.d.logger)
	if err != nil {
		t.Fatalf("SubscribeSoulForget: %v", err)
	}
	defer func() { _ = peer.Close() }()
	if err := peer.Ready(ctx); err != nil {
		t.Fatalf("peer.Ready: %v", err)
	}

	sent, err := td.Broadcast(ctx, "host1.example.com")
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if !sent {
		t.Error("Broadcast reported the notice did not go out although Redis is up")
	}

	select {
	case ev := <-peer.Channel():
		if ev == nil || ev.SID != "host1.example.com" {
			t.Fatalf("peer got %+v, want a notice for host1.example.com", ev)
		}
		if ev.OriginKID != "keeper-test-01" {
			t.Errorf("origin_kid = %q, want this instance's KID — the origin filter on the "+
				"receiving side keys off it", ev.OriginKID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Broadcast returned sent=true but no other instance received anything — " +
			"the stream of a forgotten host on another Keeper would stay open")
	}
}

// TestSoulTeardown_Broadcast_FailsWhenRedisIsUnreachable — the pre-flight in
// soulforget.Erase is built on this error existing. A Broadcast that cannot
// report a transport failure turns "nothing was deleted" into a promise the
// code no longer keeps.
func TestSoulTeardown_Broadcast_FailsWhenRedisIsUnreachable(t *testing.T) {
	td, mr := newTeardownDaemon(t, true)
	mr.Close() // the cluster is now unreachable

	sent, err := td.Broadcast(context.Background(), "host1.example.com")
	if err == nil {
		t.Fatal("Broadcast reported success against a dead Redis — soulforget.Erase would " +
			"then delete the host while a stream on an unreachable instance stayed open, " +
			"and the operator would be shown a success")
	}
	if sent {
		t.Error("Broadcast returned sent=true together with an error")
	}
}

// TestSoulTeardown_Broadcast_WithoutRedisIsNotAFailure — a Keeper without Redis
// is single-instance by construction (Redis IS the coordination layer,
// ADR-006). There is nobody to tell, which must not read as "could not tell":
// Erase treats a broadcast error as a hard stop, so an error here would make
// the operation unusable exactly where it is safest.
func TestSoulTeardown_Broadcast_WithoutRedisIsNotAFailure(t *testing.T) {
	td, _ := newTeardownDaemon(t, false)

	sent, err := td.Broadcast(context.Background(), "host1.example.com")
	if err != nil {
		t.Fatalf("Broadcast without redis: %v, want no error", err)
	}
	if sent {
		t.Error("Broadcast reported a notice went out with no Redis to send it over")
	}
}

// TestSoulTeardown_CloseLocal_CancelsTheStream — the adapter must reach the
// real StreamManager. `return true` here reports a released stream while the
// forgotten host keeps talking to this very instance.
func TestSoulTeardown_CloseLocal_CancelsTheStream(t *testing.T) {
	td, _ := newTeardownDaemon(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.d.streamManager.RegisterStream("host1.example.com", cancel)

	if !td.CloseLocal("host1.example.com") {
		t.Fatal("CloseLocal reported no stream for a SID that has one")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("CloseLocal returned true but the stream ctx was never cancelled")
	}
}

// TestSoulTeardown_CloseLocal_WithoutStreamPlaneIsNotReportedAsClosed — a
// Keeper serving the API without the gRPC stream plane holds no streams at all.
// Saying "closed" there is a release that could not have happened.
func TestSoulTeardown_CloseLocal_WithoutStreamPlaneIsNotReportedAsClosed(t *testing.T) {
	td, _ := newTeardownDaemon(t, false)
	td.d.streamManager = nil

	if td.CloseLocal("host1.example.com") {
		t.Error("CloseLocal reported closing a stream on a Keeper with no stream plane")
	}
}

// TestWatchSoulForget_ClosesTheStreamOfAHostForgottenElsewhere — the receiving
// half, and the only thing standing between "the operator's request landed on
// instance A" and "instance B keeps serving a host that no longer exists".
// soulTeardown.Broadcast is tested to SEND; this is tested to ACT on what it
// receives, over the real subscription rather than a hand-built event.
func TestWatchSoulForget_ClosesTheStreamOfAHostForgottenElsewhere(t *testing.T) {
	td, _ := newTeardownDaemon(t, true)
	d := td.d

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	d.streamManager.RegisterStream("host1.example.com", cancelStream)

	go d.watchSoulForget(ctx)

	// Publish as ANOTHER instance — the origin filters its own echo out, so a
	// notice from this KID would legitimately be ignored and prove nothing.
	deadline := time.After(5 * time.Second)
	for {
		if _, err := keeperredis.PublishSoulForget(ctx, d.redisClient, "host1.example.com", "keeper-other-02"); err != nil {
			t.Fatalf("PublishSoulForget: %v", err)
		}
		select {
		case <-streamCtx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("a host forgotten on another Keeper instance kept its stream open here — " +
				"it is erased from the registry and still being served")
		}
	}
}

// TestWatchSoulForget_SurvivesARedisFlapAndKeepsClosingStreams — the watcher
// has to outlive the connection it rides on.
//
// This subscription is the ONLY delivery path for a teardown notice: unlike
// `rbac:invalidate`, whose shape it copies, there is no TTL-poll behind it, so
// a notice that is not delivered is not repaired later — it is lost. A watcher
// that gives up on the first transport error therefore does not degrade, it
// stops: from that moment every host forgotten on another Keeper instance keeps
// a live EventStream HERE, for the life of the process, while the operator who
// forgot it was shown `broadcast: true` and no warning. Redis flapping is
// explicitly treated as routine elsewhere in this daemon (the Toll middleware
// calls it "a common phenomenon" and fails open on it), so this is not an
// exotic failure — it is the ordinary one.
//
// The flap is real, not simulated: Redis is stopped and brought back on the
// SAME address, which is what a restart or a failover looks like to the client.
// A notice published after it must still close a stream.
func TestWatchSoulForget_SurvivesARedisFlapAndKeepsClosingStreams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mr := miniredis.NewMiniRedis()
	if err := mr.StartAddr("127.0.0.1:0"); err != nil {
		t.Fatalf("miniredis StartAddr: %v", err)
	}
	addr := mr.Addr()
	defer mr.Close()

	client, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: addr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	d := &daemon{
		logger:        logger,
		cfg:           &config.KeeperConfig{KID: "keeper-test-01"},
		streamManager: keepergrpc.NewStreamManager(logger),
		redisClient:   client,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go d.watchSoulForget(ctx)

	// Before the flap — proves the watcher was actually delivering, so a failure
	// after the flap is the flap and not a broken fixture.
	awaitForgottenStreamClosed(t, ctx, d, "host1.example.com", 10*time.Second,
		"the watcher never closed a stream even before the flap — the fixture is broken, not the code")

	mr.Close()
	// Let the subscription actually observe the drop before Redis returns;
	// without this the test could pass on a connection that never broke.
	time.Sleep(500 * time.Millisecond)

	revived := miniredis.NewMiniRedis()
	if err := revived.StartAddr(addr); err != nil {
		t.Fatalf("miniredis restart on %s: %v", addr, err)
	}
	defer revived.Close()

	awaitForgottenStreamClosed(t, ctx, d, "host2.example.com", 25*time.Second,
		"Redis flapped and came back, and the watcher never reconnected: every host "+
			"forgotten elsewhere from now on keeps its stream on this instance for the life "+
			"of the process, and the operator who forgot it was shown a success")
}

// awaitForgottenStreamClosed registers a stream for sid, then republishes a
// foreign-origin forget notice until the stream is closed or the budget runs
// out. Republishing is not slack: pub/sub has no replay, so a notice sent
// before the subscriber is attached is simply gone — the loop is how the test
// distinguishes "not subscribed yet" from "will never subscribe again".
func awaitForgottenStreamClosed(t *testing.T, ctx context.Context, d *daemon, sid string, budget time.Duration, failMsg string) {
	t.Helper()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	d.streamManager.RegisterStream(sid, cancelStream)

	deadline := time.After(budget)
	for {
		// A publish error is expected while Redis is down or re-dialling; the
		// deadline is the judge, not any single attempt.
		_, _ = keeperredis.PublishSoulForget(ctx, d.redisClient, sid, "keeper-other-02")
		select {
		case <-streamCtx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatal(failMsg)
		}
	}
}

// TestWatchSoulForget_IgnoresThisInstancesOwnNotice — the origin closes its own
// stream synchronously inside soulforget.Erase (NIM-421: the node that acted
// must not learn about its own action from the wire). Acting on the echo would
// be redundant at best; at worst it is the thing the origin waits for instead
// of acting.
func TestWatchSoulForget_IgnoresThisInstancesOwnNotice(t *testing.T) {
	td, _ := newTeardownDaemon(t, true)
	d := td.d

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	d.streamManager.RegisterStream("host1.example.com", cancelStream)

	go d.watchSoulForget(ctx)
	time.Sleep(300 * time.Millisecond) // let the subscription settle

	for i := 0; i < 5; i++ {
		if _, err := keeperredis.PublishSoulForget(ctx, d.redisClient, "host1.example.com", d.cfg.KID); err != nil {
			t.Fatalf("PublishSoulForget: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case <-streamCtx.Done():
		t.Error("the watcher acted on this instance's own notice; the origin already closed " +
			"its stream inside Erase and must not be driven by its own echo")
	default:
	}
}
