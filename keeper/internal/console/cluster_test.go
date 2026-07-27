package console

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Cross-Keeper routing, end to end over a real (in-memory) Redis.
//
// The case these cover is the NORMAL one in a cluster, not an edge: the
// operator's socket terminates on whichever Keeper the load balancer picked,
// while the host's EventStream is held by whichever won the SoulLease. With N
// instances they coincide about 1/N of the time, so without this path a console
// would simply go silent for most operators.

func newSharedRedis(t *testing.T) *keeperredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// keeperNode is one instance of the cluster in a test.
type keeperNode struct {
	hub    *Hub
	bridge *ClusterBridge
	disp   *recordingDispatcher
	// stop tears down this node's upstream subscription — how a test kills an
	// instance while its claim is still warm in Redis.
	stop func()
}

func newKeeperNode(t *testing.T, rdb *keeperredis.Client, kid string) *keeperNode {
	t.Helper()
	bridge := NewClusterBridge(rdb, kid, testLogger())
	if bridge == nil {
		t.Fatal("NewClusterBridge returned nil with a live redis client")
	}
	disp := &recordingDispatcher{}
	hub, err := NewHub(HubDeps{Dispatcher: disp, Cluster: bridge, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return &keeperNode{hub: hub, bridge: bridge, disp: disp}
}

// startUpstream subscribes a node to its own console channel and pumps frames
// into its Hub — what the daemon does at startup.
func (n *keeperNode) startUpstream(t *testing.T, rdb *keeperredis.Client, kid string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := keeperredis.SubscribeConsoleUpstream(ctx, rdb, kid, testLogger())
	if err != nil {
		cancel()
		t.Fatalf("SubscribeConsoleUpstream: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		cancel()
		t.Fatalf("Ready: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUpstreamConsumer(ctx, sub, n.hub)
	}()
	stop := func() {
		cancel()
		_ = sub.Close()
		<-done
	}
	n.stop = stop
	t.Cleanup(func() {
		if n.stop != nil {
			n.stop()
			n.stop = nil
		}
	})
}

// stopUpstream simulates this instance dying: its channel subscription goes
// away while the claim it wrote is still live in Redis.
func (n *keeperNode) stopUpstream(t *testing.T) {
	t.Helper()
	if n.stop == nil {
		t.Fatal("stopUpstream called without startUpstream")
	}
	n.stop()
	n.stop = nil
}

func waitSink(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The operator's socket is on node A; the host's EventStream on node B. Output
// arriving at B must reach the sink on A.
func TestCluster_UpstreamReachesTheSocketOwner(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sink := &captureSink{}
	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", sink)

	// Node B holds the stream and knows nothing about this session locally.
	if nodeB.hub.Count() != 0 {
		t.Fatal("test setup: node B must not hold the session")
	}

	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID, Pid: 321,
		}},
	})
	waitSink(t, "opened to cross the bridge", func() bool {
		opened, _, _, _ := sink.counts()
		return opened == 1
	})

	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID,
			Stream:    keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT,
			Data:      []byte{0x1b, '[', '0', 'm', 0x00, 0xff},
		}},
	})
	waitSink(t, "a chunk to cross the bridge", func() bool {
		_, chunks, _, _ := sink.counts()
		return chunks == 1
	})

	sink.mu.Lock()
	if got := sink.chunks[0].SessionID; got != "pane-1" {
		t.Fatalf("bridged chunk addressed to %q, want the client id pane-1", got)
	}
	sink.mu.Unlock()

	// The terminal frame must cross too, or the pane would stay live forever.
	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: sess.KeeperID,
			ExitCode:  0,
			Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
		}},
	})
	waitSink(t, "exit to cross the bridge", func() bool {
		_, _, exits, _ := sink.counts()
		return exits == 1
	})
	waitSink(t, "the owner to release the session", func() bool { return nodeA.hub.Count() == 0 })
}

// The claim is what makes the session findable from another instance; without
// it the frame has nowhere to go.
func TestCluster_ClaimIsPublishedAndReleased(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-a")
	ctx := context.Background()

	sess := mustOpen(t, node.hub, "pane-1", "host-x", "archon-a", &captureSink{})

	owner, err := keeperredis.ReadConsoleSessionOwner(ctx, rdb, sess.KeeperID)
	if err != nil {
		t.Fatalf("ReadConsoleSessionOwner: %v", err)
	}
	if owner != "kid-a" {
		t.Fatalf("claim owner = %q, want kid-a", owner)
	}

	node.hub.Close(ctx, sess, "operator detached")
	owner, _ = keeperredis.ReadConsoleSessionOwner(ctx, rdb, sess.KeeperID)
	if owner != "" {
		t.Fatalf("claim owner after close = %q, want it released", owner)
	}
}

// A holder must resolve the owner ONCE and cache it: a Redis GET per pty chunk
// would put the cluster's cache in the path of every keystroke echo.
func TestCluster_OwnerLookupIsCachedPerSession(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sink := &captureSink{}
	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", sink)

	send := func() {
		nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId: sess.KeeperID, Data: []byte("x"),
			}},
		})
	}

	send()
	waitSink(t, "the first chunk to arrive", func() bool {
		_, chunks, _, _ := sink.counts()
		return chunks == 1
	})

	// Delete the claim: a cached route must keep working, which is the whole
	// point of the short claim TTL.
	if err := keeperredis.ReleaseConsoleSession(context.Background(), rdb, sess.KeeperID); err != nil {
		t.Fatalf("ReleaseConsoleSession: %v", err)
	}

	send()
	waitSink(t, "a chunk to arrive from cache after the claim is gone", func() bool {
		_, chunks, _, _ := sink.counts()
		return chunks == 2
	})
}

// A frame for a session nobody claims must be dropped, not retried forever.
func TestCluster_UnknownSessionIsDropped(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-b")

	node.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: "01JNOBODYOWNSTHISSESSION00", Data: []byte("orphan"),
		}},
	})

	// Nothing to assert beyond "it did not block or panic"; the negative-lookup
	// cache is what keeps the next thousand chunks from hammering Redis.
	if node.hub.Count() != 0 {
		t.Fatalf("live sessions = %d, want 0", node.hub.Count())
	}
}

// A frame that arrives over the bridge for a session that is NOT ours must not
// be re-forwarded, or two instances would bounce it between each other.
func TestCluster_DeliverLocalDoesNotReforward(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeB.startUpstream(t, rdb, "kid-b")

	sink := &captureSink{}
	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", sink)

	// Node B receives a bridged frame for node A's session — a stale route.
	nodeB.hub.DeliverLocal(context.Background(), &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("bounce"),
		}},
	})

	// It must be dropped where it landed, never published onward to kid-a.
	time.Sleep(100 * time.Millisecond)
	if _, chunks, _, _ := sink.counts(); chunks != 0 {
		t.Fatalf("the owner received %d bridged chunks — DeliverLocal re-forwarded and the frame looped", chunks)
	}
}

// --- orphan reaping ---
//
// The cluster-mode hole in kill-on-disconnect: when the Keeper HOLDING an
// operator socket dies, the EventStream on the other instance never breaks, so
// the Soul's own kill-on-disconnect never fires and the pty keeps running with
// nobody watching it. Found on a two-keeper live stand, where a shell survived
// its owner's death. The stream holder is the only party left that can see the
// orphan, so it is the one that reaps.

// waitDispatch polls for a dispatched ConsoleClose.
func waitDispatch(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCluster_OrphanedSessionIsReapedWhenOwnerDies(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a") // owns the operator socket
	nodeB := newKeeperNode(t, rdb, "kid-b") // holds the host's EventStream
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", &captureSink{})

	// Node A dies: its claim survives in Redis for the TTL, but nothing is
	// subscribed to its channel any more.
	nodeA.stopUpstream(t)

	// Keep feeding output while the broker settles the unsubscribe — the reap
	// fires on the first frame that finds zero subscribers, and the cooldown
	// makes every later frame a no-op, so re-sending cannot inflate the count.
	waitDispatch(t, "the stream holder to reap the orphan", func() bool {
		nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId: sess.KeeperID, Data: []byte("output nobody is reading"),
			}},
		})
		return nodeB.disp.closeCount() == 1
	})
	nodeB.disp.mu.Lock()
	defer nodeB.disp.mu.Unlock()
	if got := nodeB.disp.closes[0].GetSessionId(); got != sess.KeeperID {
		t.Fatalf("reaped session %q, want %q", got, sess.KeeperID)
	}
}

// A session with no claim at all is an orphan too — the owner released it and
// died, or never claimed. Either way the pty has no audience.
func TestCluster_UnclaimedSessionIsReaped(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-b")

	node.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: "01JNOBODYOWNSTHISSESSION00", Data: []byte("orphan"),
		}},
	})

	waitDispatch(t, "the unclaimed session to be reaped", func() bool {
		return node.disp.closeCount() == 1
	})
}

// Output arrives as a stream; a dead owner must produce ONE reap, not one per
// chunk, or the shared EventStream would drown in ConsoleClose messages.
func TestCluster_OrphanReapIsSentOnce(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-b")

	for i := 0; i < 50; i++ {
		node.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId: "01JNOBODYOWNSTHISSESSION00", Data: []byte("flood"),
			}},
		})
	}
	waitDispatch(t, "the first reap", func() bool { return node.disp.closeCount() >= 1 })
	time.Sleep(200 * time.Millisecond)

	if got := node.disp.closeCount(); got != 1 {
		t.Fatalf("dispatched %d ConsoleClose for one orphan, want exactly 1", got)
	}
}

// A terminal frame needs no reap: the session is ending on its own, and closing
// it again would be noise on the stream.
func TestCluster_TerminalFrameIsNotReaped(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-b")

	node.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: "01JNOBODYOWNSTHISSESSION00",
			Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
		}},
	})
	time.Sleep(200 * time.Millisecond)

	if got := node.disp.closeCount(); got != 0 {
		t.Fatalf("dispatched %d ConsoleClose for a terminal frame, want 0", got)
	}
}

// The one thing worse than an orphan is killing a healthy operator's shell. A
// Redis failure says NOTHING about the owner and must never be read as "gone".
func TestCluster_LookupFailureDoesNotReap(t *testing.T) {
	rdb := newSharedRedis(t)
	node := newKeeperNode(t, rdb, "kid-b")

	// Break the client: every lookup now errors instead of answering "no claim".
	_ = rdb.Close()

	node.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: "01JSOMEONESLIVESESSION0000", Data: []byte("output"),
		}},
	})
	time.Sleep(300 * time.Millisecond)

	if got := node.disp.closeCount(); got != 0 {
		t.Fatalf("dispatched %d ConsoleClose on a Redis failure — a live shell could be killed by a cache blip", got)
	}
}

// A session whose owner is alive and subscribed must never be reaped.
func TestCluster_LiveOwnerIsNotReaped(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sink := &captureSink{}
	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", sink)

	for i := 0; i < 10; i++ {
		nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId: sess.KeeperID, Data: []byte("live output"),
			}},
		})
	}
	waitSink(t, "chunks to reach the live owner", func() bool {
		_, chunks, _, _ := sink.counts()
		return chunks == 10
	})

	if got := nodeB.disp.closeCount(); got != 0 {
		t.Fatalf("dispatched %d ConsoleClose against a HEALTHY session", got)
	}
}

// The dangerous orphan is the QUIET one. A session doing `sleep 900` produces
// no output at all, so the forward-path check never fires — nothing would ever
// notice, and the pty would outlive every Keeper that knew about it. This is
// the hole the live two-keeper stand exposed after the reactive reap was in.
func TestCluster_QuietOrphanIsReapedByTheSweep(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a") // owns the socket
	nodeB := newKeeperNode(t, rdb, "kid-b") // holds the EventStream
	nodeA.startUpstream(t, rdb, "kid-a")

	sink := &captureSink{}
	sess := mustOpen(t, nodeA.hub, "pane-1", "host-quiet", "archon-a", sink)

	// One frame goes through, which is how the holder learns the route — in
	// production this is ConsoleOpened, which every session emits.
	nodeB.hub.Deliver(context.Background(), "host-quiet", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID, Pid: 777,
		}},
	})
	waitSink(t, "opened to cross the bridge", func() bool {
		opened, _, _, _ := sink.counts()
		return opened == 1
	})

	// The owner dies and its claim lapses. From here the session is silent —
	// no chunk will ever arrive to trigger the reactive path.
	nodeA.stopUpstream(t)
	if err := keeperredis.ReleaseConsoleSession(context.Background(), rdb, sess.KeeperID); err != nil {
		t.Fatalf("ReleaseConsoleSession: %v", err)
	}

	if n := nodeB.hub.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("swept %d orphans, want 1 — a silent abandoned shell would survive", n)
	}
	if nodeB.disp.closeCount() != 1 {
		t.Fatalf("dispatched %d ConsoleClose, want 1", nodeB.disp.closeCount())
	}
	nodeB.disp.mu.Lock()
	defer nodeB.disp.mu.Unlock()
	if got := nodeB.disp.closes[0].GetSessionId(); got != sess.KeeperID {
		t.Fatalf("reaped %q, want %q", got, sess.KeeperID)
	}
}

// The sweep must address the reap to the right host: an upstream frame carries
// only a session id, so the SID has to be remembered when the route is learned.
func TestCluster_SweepReapsAgainstTheRightHost(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "db-07.example.com", "archon-a", &captureSink{})
	nodeB.hub.Deliver(context.Background(), "db-07.example.com", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID,
		}},
	})
	waitDispatch(t, "the route to be learned", func() bool { return nodeB.bridge.tracks(sess.KeeperID) })

	nodeA.stopUpstream(t)
	_ = keeperredis.ReleaseConsoleSession(context.Background(), rdb, sess.KeeperID)
	nodeB.hub.SweepOrphans(context.Background())

	nodeB.disp.mu.Lock()
	defer nodeB.disp.mu.Unlock()
	if len(nodeB.disp.closes) != 1 {
		t.Fatalf("dispatched %d closes, want 1", len(nodeB.disp.closes))
	}
	if got := nodeB.disp.closeSIDs[0]; got != "db-07.example.com" {
		t.Fatalf("reap addressed to host %q, want db-07.example.com", got)
	}
}

// A live owner keeps refreshing its claim; the sweep must leave it alone.
func TestCluster_SweepSparesLiveOwners(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", &captureSink{})
	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID,
		}},
	})
	waitDispatch(t, "the route to be learned", func() bool { return nodeB.bridge.tracks(sess.KeeperID) })

	for i := 0; i < 3; i++ {
		if n := nodeB.hub.SweepOrphans(context.Background()); n != 0 {
			t.Fatalf("swept %d sessions with a healthy owner, want 0", n)
		}
	}
	if nodeB.disp.closeCount() != 0 {
		t.Fatalf("dispatched %d ConsoleClose against a live console", nodeB.disp.closeCount())
	}
}

// A Redis failure must never be read as "the owner is gone".
func TestCluster_SweepDoesNotReapOnLookupFailure(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", &captureSink{})
	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID,
		}},
	})
	waitDispatch(t, "the route to be learned", func() bool { return nodeB.bridge.tracks(sess.KeeperID) })

	_ = rdb.Close() // every lookup now errors instead of answering

	if n := nodeB.hub.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("swept %d sessions on a Redis failure — a cache blip must not kill live shells", n)
	}
}

// A dying pty usually emits a parting chunk, which would hit the forward path
// right after the sweep already reaped the session. One reap is enough.
func TestCluster_SweepAndForwardPathDoNotDoubleReap(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	nodeB := newKeeperNode(t, rdb, "kid-b")
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", &captureSink{})
	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID,
		}},
	})
	waitDispatch(t, "the route to be learned", func() bool { return nodeB.bridge.tracks(sess.KeeperID) })

	nodeA.stopUpstream(t)
	_ = keeperredis.ReleaseConsoleSession(context.Background(), rdb, sess.KeeperID)

	if n := nodeB.hub.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("sweep reaped %d, want 1", n)
	}
	// The parting output of the shell the sweep just killed.
	nodeB.hub.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("goodbye"),
		}},
	})
	time.Sleep(200 * time.Millisecond)

	if got := nodeB.disp.closeCount(); got != 1 {
		t.Fatalf("dispatched %d ConsoleClose for one orphan, want exactly 1", got)
	}
}

// A reap must close the audit trail too. The Keeper that opened the session —
// the one that knew the Archon — is the one that died, so without this the log
// would keep a `console.opened` with no matching close.
func TestCluster_OrphanReapIsAudited(t *testing.T) {
	rdb := newSharedRedis(t)
	nodeA := newKeeperNode(t, rdb, "kid-a")
	audits := &captureAudit{}
	bridge := NewClusterBridge(rdb, "kid-b", testLogger())
	disp := &recordingDispatcher{}
	nodeB, err := NewHub(HubDeps{
		Dispatcher: disp, Cluster: bridge, AuditWriter: audits, Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	nodeA.startUpstream(t, rdb, "kid-a")

	sess := mustOpen(t, nodeA.hub, "pane-1", "host-x", "archon-a", &captureSink{})
	nodeB.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID,
		}},
	})
	waitDispatch(t, "the route to be learned", func() bool { return bridge.tracks(sess.KeeperID) })

	nodeA.stopUpstream(t)
	_ = keeperredis.ReleaseConsoleSession(context.Background(), rdb, sess.KeeperID)
	if n := nodeB.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	events := audits.events()
	if len(events) != 1 {
		t.Fatalf("wrote %d audit events, want 1", len(events))
	}
	ev := events[0]
	if ev.EventType != audit.EventConsoleClosed {
		t.Fatalf("event type = %q, want console.closed", ev.EventType)
	}
	if ev.CorrelationID != sess.KeeperID {
		t.Fatalf("correlation_id = %q, want the session id — it is what ties the close to its open", ev.CorrelationID)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Fatalf("source = %q, want keeper_internal (no operator is present to attribute it to)", ev.Source)
	}
	if ev.Payload["sid"] != "host-x" {
		t.Fatalf("payload sid = %v, want host-x", ev.Payload["sid"])
	}
}

// captureAudit records what the Hub wrote to the audit log.
type captureAudit struct {
	mu   sync.Mutex
	seen []audit.Event
}

func (c *captureAudit) Write(_ context.Context, ev *audit.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, *ev)
	return nil
}

func (c *captureAudit) events() []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]audit.Event(nil), c.seen...)
}
