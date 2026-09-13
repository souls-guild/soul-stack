package pluginhost

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
)

// keeperSideDoc builds a soul_module document whose single module declares
// side. Passing "" is the artifact written before [ADR-0087] existed — the key
// absent, which must read as `soul`.
func keeperSideDoc(module string, side schema.Side) schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:   module,
			Side:   side,
			States: map[string]schema.State{"created": {Description: "test state"}},
		}},
	}
}

func keeperSideEntry(alias, module string, side schema.Side) Discovered {
	doc := keeperSideDoc(module, side)
	return Discovered{
		Alias:      alias,
		Module:     module,
		Doc:        &doc,
		BinaryPath: "/bogus/" + alias,
		Dir:        "/bogus",
	}
}

// GUARD: only `side: keeper` is executable here, and the two ways of NOT saying
// so — the absent key and an explicit `soul` — are the same answer. A module
// reachable through LookupKeeperSide is one the Keeper is about to run in its
// own process tree, so this is the boundary the whole path rests on.
func TestKeeperSideModules_IndexesOnlyKeeperSide(t *testing.T) {
	r := NewKeeperSideModules(&fakeSoulModuleSpawner{}, []Discovered{
		keeperSideEntry("wbcloud", "vm", schema.SideKeeper),
		keeperSideEntry("redis", "acl", schema.SideSoul),
		keeperSideEntry("legacy", "thing", ""),
	}, nil)

	if _, ok := r.LookupKeeperSide("wbcloud.vm"); !ok {
		t.Error("LookupKeeperSide(wbcloud.vm): not found, want the side: keeper module")
	}
	for _, addr := range []string{"redis.acl", "legacy.thing"} {
		if _, ok := r.LookupKeeperSide(addr); ok {
			t.Errorf("LookupKeeperSide(%s): found — a Soul-side module must not be executable on the keeper", addr)
		}
	}
	if got := r.Names(); len(got) != 1 || got[0] != "wbcloud.vm" {
		t.Errorf("Names() = %v, want exactly [wbcloud.vm]", got)
	}
}

// GUARD: DeclaredSide tells "runs on the Soul" apart from "no such module".
// Collapsing the two is what sends an author hunting a typo in an address that
// is spelled correctly.
func TestKeeperSideModules_DeclaredSideDistinguishesUnknown(t *testing.T) {
	r := NewKeeperSideModules(&fakeSoulModuleSpawner{}, []Discovered{
		keeperSideEntry("redis", "acl", schema.SideSoul),
		keeperSideEntry("legacy", "thing", ""),
	}, nil)

	for _, addr := range []string{"redis.acl", "legacy.thing"} {
		side, known := r.DeclaredSide(addr)
		if !known {
			t.Errorf("DeclaredSide(%s): known=false, want a registered module", addr)
		}
		if side != schema.SideSoul {
			t.Errorf("DeclaredSide(%s) = %q, want soul (the absent key reads as soul)", addr, side)
		}
	}
	if _, known := r.DeclaredSide("nothing.here"); known {
		t.Error("DeclaredSide(nothing.here): known=true, want false")
	}
}

// An ssh_provider entry carries no modules[] at all; it must not become
// addressable as a keeper-side module through some other door.
func TestKeeperSideModules_SkipsOtherKinds(t *testing.T) {
	doc := schema.Document{Kind: schema.KindSSHProvider, ProtocolVersion: 1}
	r := NewKeeperSideModules(&fakeSoulModuleSpawner{}, []Discovered{
		{Alias: "aws", Doc: &doc, BinaryPath: "/bogus/aws", Dir: "/bogus"},
	}, nil)
	if len(r.Names()) != 0 {
		t.Errorf("Names() = %v, want empty (ssh_provider is not a keeper-side module)", r.Names())
	}
	if _, known := r.DeclaredSide("aws"); known {
		t.Error("DeclaredSide(aws): known=true — a non-soul_module kind must not be indexed")
	}
}

func TestKeeperSideModules_ApplySpawnsForwardsAndCloses(t *testing.T) {
	spawner := &fakeSoulModuleSpawner{
		session: &fakeSoulModuleSession{events: []*pluginv1.ApplyEvent{
			{Message: "provisioning"},
			{Changed: true, Message: "done"},
		}},
	}
	r := NewKeeperSideModules(spawner, []Discovered{
		keeperSideEntry("wbcloud", "vm", schema.SideKeeper),
	}, nil)
	mod, ok := r.LookupKeeperSide("wbcloud.vm")
	if !ok {
		t.Fatal("LookupKeeperSide(wbcloud.vm): not found")
	}

	stream := &collectingApplyStream{ctx: context.Background()}
	if err := mod.Apply(&pluginv1.ApplyRequest{State: "created"}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(stream.events) != 2 {
		t.Fatalf("forwarded events = %d, want 2 (every ApplyEvent reaches the caller's stream)", len(stream.events))
	}
	if spawner.spawnCount != 1 {
		t.Errorf("spawnCount = %d, want 1 (one-shot per Apply, ADR-020(d))", spawner.spawnCount)
	}
	if spawner.lastDiscovered.Module != "vm" || spawner.lastDiscovered.Alias != "wbcloud" {
		t.Errorf("spawned %s.%s, want wbcloud.vm", spawner.lastDiscovered.Alias, spawner.lastDiscovered.Module)
	}
	if !spawner.session.closed {
		t.Error("session not closed after Apply — a leaked plugin process per keeper-side step")
	}
}

// --- fakes ---

type fakeSoulModuleSpawner struct {
	session        *fakeSoulModuleSession
	spawnErr       error
	spawnCount     int
	lastDiscovered Discovered
}

func (f *fakeSoulModuleSpawner) SpawnSoulModule(_ context.Context, d Discovered) (SoulModuleSession, error) {
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	f.spawnCount++
	f.lastDiscovered = d
	if f.session == nil {
		f.session = &fakeSoulModuleSession{}
	}
	return f.session, nil
}

type fakeSoulModuleSession struct {
	events   []*pluginv1.ApplyEvent
	applyErr error
	closed   bool
}

func (f *fakeSoulModuleSession) Apply(_ context.Context, _ *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &fakeApplyStreamClient{events: f.events}, nil
}

func (f *fakeSoulModuleSession) Close() error {
	f.closed = true
	return nil
}

type fakeApplyStreamClient struct {
	grpc.ClientStream
	events []*pluginv1.ApplyEvent
	idx    int
}

func (c *fakeApplyStreamClient) Recv() (*pluginv1.ApplyEvent, error) {
	if c.idx >= len(c.events) {
		return nil, io.EOF
	}
	ev := c.events[c.idx]
	c.idx++
	return ev, nil
}

func (c *fakeApplyStreamClient) Header() (metadata.MD, error) { return nil, nil }
func (c *fakeApplyStreamClient) Trailer() metadata.MD         { return nil }
func (c *fakeApplyStreamClient) CloseSend() error             { return nil }
func (c *fakeApplyStreamClient) Context() context.Context     { return context.Background() }
func (c *fakeApplyStreamClient) SendMsg(any) error            { return nil }

// RecvMsg stays a no-op: the apply cycle only calls Recv, and copying the proto
// message (which carries protoimpl.MessageState) would trip go vet.
func (c *fakeApplyStreamClient) RecvMsg(any) error { return nil }

// collectingApplyStream is the caller's side of the forward — a minimal
// grpc.ServerStreamingServer that keeps what it was sent.
type collectingApplyStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.ApplyEvent
}

func (s *collectingApplyStream) Context() context.Context { return s.ctx }

func (s *collectingApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *collectingApplyStream) SetHeader(metadata.MD) error  { return nil }
func (s *collectingApplyStream) SendHeader(metadata.MD) error { return nil }
func (s *collectingApplyStream) SetTrailer(metadata.MD)       {}
func (s *collectingApplyStream) SendMsg(any) error            { return nil }
func (s *collectingApplyStream) RecvMsg(any) error            { return nil }
