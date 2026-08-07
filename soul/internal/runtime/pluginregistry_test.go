package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestPluginRegistry_LookupKnown(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	r := NewPluginRegistry(&fakeSpawner{}, []pluginhost.Discovered{d}, nil)
	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Fatal("Lookup(acme.echo): not found")
	}
	if _, ok := r.Lookup("acme.unknown"); ok {
		t.Fatal("Lookup(acme.unknown): unexpectedly found")
	}
}

func TestPluginRegistry_ApplySpawnsAndCloses(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{
				events: []*pluginv1.ApplyEvent{
					{Message: "starting"},
					{Changed: true, Output: mustStruct(nil, map[string]any{"hello": "world"})},
				},
			}
		},
	}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	stream := newInProcApplyStream(context.Background())
	if err := mod.Apply(&pluginv1.ApplyRequest{State: "applied"}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(stream.events) != 2 {
		t.Fatalf("events received = %d, want 2", len(stream.events))
	}
	if spawner.spawnCount != 1 {
		t.Errorf("spawnCount = %d, want 1 (one-shot per Apply)", spawner.spawnCount)
	}
	if !spawner.lastSession.closed {
		t.Error("session not closed after Apply")
	}
}

func TestPluginRegistry_ApplyRpcErrorPropagates(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{applyErr: errors.New("rpc broken")}
		},
	}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	stream := newInProcApplyStream(context.Background())
	err := mod.Apply(&pluginv1.ApplyRequest{}, stream)
	if err == nil {
		t.Fatal("expected error from RPC failure")
	}
}

func TestPluginRegistry_SpawnErrorPropagates(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{spawnErr: errors.New("plugin not found")}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	err := mod.Apply(&pluginv1.ApplyRequest{}, newInProcApplyStream(context.Background()))
	if err == nil {
		t.Fatal("expected error from Spawn failure")
	}
}

func TestCompositeRegistry_CoreShadowsPlugin(t *testing.T) {
	core := mapRegistry{"core.pkg": &fakeModule{}}
	plug := mapRegistry{"core.pkg": &fakeModule{}, "acme.echo": &fakeModule{}}

	c := NewCompositeRegistry(core, plug)
	got, ok := c.Lookup("core.pkg")
	if !ok || got != core["core.pkg"] {
		t.Errorf("Lookup(core.pkg): expected core layer to win")
	}
	if _, ok := c.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo): plugin layer should be reachable")
	}
	if _, ok := c.Lookup("core.frobnicate"); ok {
		t.Error("Lookup(unknown): should be false")
	}
}

func TestRun_DispatchesToPluginViaComposite(t *testing.T) {
	// Integration of applyrunner ↔ pluginregistry: module acme.echo isn't in
	// core but is in the plugin layer; ApplyRunner finds it via composite.
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{events: []*pluginv1.ApplyEvent{{Changed: true}}}
		},
	}
	composite := NewCompositeRegistry(
		mapRegistry{},
		NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil),
	)
	r := NewApplyRunner(composite, nil)
	sink := &recordingSink{}

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "plug-1",
		Tasks:   []*keeperv1.RenderedTask{{Module: "acme.echo.applied"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v", sink.taskEvents[0].GetStatus())
	}
	if spawner.spawnCount != 1 {
		t.Errorf("spawnCount = %d", spawner.spawnCount)
	}
}

// GUARD: one artifact serving several modules registers one address per module, and
// the level-1 name comes from the slot the operator named. Two registrations of the
// same bundle therefore coexist without either shadowing the other.
func TestPluginRegistry_BundleRegistersOneAddressPerModule(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "redis", "acl", "config")
	writeSlot(t, root, "redis-community", "acl", "config")

	discovered, warns, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("discovery warnings: %v", warns)
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)

	for _, want := range []string{
		"redis.acl", "redis.config",
		"redis-community.acl", "redis-community.config",
	} {
		if _, ok := r.Lookup(want); !ok {
			t.Errorf("Lookup(%s): not found; registered %v", want, r.Names())
		}
	}
	if got := len(r.Names()); got != 4 {
		t.Errorf("registered %d addresses, want 4: %v", got, r.Names())
	}
}

// GUARD: a spawn of `redis.acl` reaches the artifact with module `acl` — the registry
// hands the spawner the entry it resolved, and the module name on it is what argv
// carries.
func TestPluginRegistry_ApplySpawnsTheAddressedModule(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "redis", "acl", "config")
	discovered, _, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	spawner := &fakeSpawner{makeSession: func() *fakeSession {
		return &fakeSession{events: []*pluginv1.ApplyEvent{{Changed: true}}}
	}}
	r := NewPluginRegistry(spawner, discovered, nil)

	mod, ok := r.Lookup("redis.config")
	if !ok {
		t.Fatal("Lookup(redis.config): not found")
	}
	if err := mod.Apply(&pluginv1.ApplyRequest{State: "applied"}, newInProcApplyStream(context.Background())); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if spawner.lastDiscovered.Module != "config" {
		t.Errorf("spawned module = %q, want config", spawner.lastDiscovered.Module)
	}
	if spawner.lastDiscovered.Alias != "redis" {
		t.Errorf("spawned alias = %q, want redis", spawner.lastDiscovered.Alias)
	}
}

// GUARD: StateInput reads the addressed module's states, never a sibling's. Both
// modules of the bundle declare a state called `applied` with different inputs; asking
// for one must not return the other's contract.
func TestPluginRegistry_StateInputIsPerModule(t *testing.T) {
	doc := testDocument("acl", "config")
	for i := range doc.Modules {
		doc.Modules[i].States = map[string]schema.State{
			"applied": {Description: "x", Input: schema.Input{doc.Modules[i].Name + "_param": {Type: schema.String}}},
		}
	}
	var discovered []pluginhost.Discovered
	for _, m := range doc.Modules {
		discovered = append(discovered, pluginhost.Discovered{
			Alias: "redis", Module: m.Name, Doc: &doc, BinaryPath: "/bogus/redis", Dir: "/bogus",
		})
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)

	in, strictness := r.StateInput("redis.acl", "applied")
	if strictness != ParamsEnforced {
		t.Fatalf("strictness = %v, want ParamsEnforced", strictness)
	}
	if _, ok := in["acl_param"]; !ok {
		t.Errorf("redis.acl contract = %v, want its own acl_param", in)
	}
	if _, leaked := in["config_param"]; leaked {
		t.Error("a sibling module's parameter leaked into the contract")
	}
}

// --- Rescan (hot-register, ADR-065(d)) ---

func TestPluginRegistry_RescanPicksUpNewModule(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "acme", "echo")
	discovered, _, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)
	if _, ok := r.Lookup("acme-extra.extra"); ok {
		t.Fatal("Lookup(acme-extra.extra): found before installation")
	}

	writeSlot(t, root, "acme-extra", "extra")
	if _, err := r.Rescan(root); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, ok := r.Lookup("acme-extra.extra"); !ok {
		t.Error("Lookup(acme-extra.extra) after Rescan: not found")
	}
	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo): existing module lost after Rescan")
	}
}

func TestCompositeRegistry_RescanKeepsCoreLayer(t *testing.T) {
	root := t.TempDir()
	plug := NewPluginRegistry(&fakeSpawner{}, nil, nil)
	core := mapRegistry{"core.pkg": &fakeModule{}}
	c := NewCompositeRegistry(core, plug)

	writeSlot(t, root, "acme-extra", "extra")
	if _, err := plug.Rescan(root); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	got, ok := c.Lookup("core.pkg")
	if !ok || got != core["core.pkg"] {
		t.Error("core-layer composite affected by plugin-layer Rescan")
	}
	if _, ok := c.Lookup("acme-extra.extra"); !ok {
		t.Error("new module unavailable via composite after Rescan")
	}
}

func TestPluginRegistry_ConcurrentLookupAndRescan(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "acme", "echo")
	discovered, _, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Lookup("acme.echo")
					r.Names()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if _, err := r.Rescan(root); err != nil {
			t.Errorf("Rescan #%d: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()

	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo) after concurrent Rescans: not found")
	}
}

// --- helpers ---

// writeSlot materializes a valid slot `<root>/<alias>/` — one executable named by the
// alias, with a stamped schema document declaring the given modules. This is the
// core.module.installed format, and going through the real stamping keeps the fixture
// from drifting away from what a host actually reads.
func writeSlot(t *testing.T, root, alias string, modules ...string) {
	t.Helper()
	dir := filepath.Join(root, alias)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, alias)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload, err := schema.Marshal(testDocument(modules...))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.WriteTrailerFile(path, payload); err != nil {
		t.Fatal(err)
	}
}

// testDocument builds a kind=soul_module document over the named modules, each with a
// single `applied` state.
func testDocument(modules ...string) schema.Document {
	doc := schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1}
	for _, name := range modules {
		doc.Modules = append(doc.Modules, schema.Module{
			Name:        name,
			Description: name,
			States: map[string]schema.State{
				"applied": {Description: "test state"},
			},
		})
	}
	return doc
}

// makeDiscovered builds an in-memory entry for `<alias>.<module>` without touching
// disk — for the registry tests, which never spawn anything real.
func makeDiscovered(alias, module string) pluginhost.Discovered {
	doc := testDocument(module)
	return pluginhost.Discovered{
		Alias:      alias,
		Module:     module,
		Doc:        &doc,
		BinaryPath: "/bogus/" + alias,
		Dir:        "/bogus",
	}
}

type fakeSpawner struct {
	makeSession    func() *fakeSession
	spawnErr       error
	spawnCount     int
	lastSession    *fakeSession
	lastDiscovered pluginhost.Discovered
}

func (f *fakeSpawner) Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error) {
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	f.spawnCount++
	f.lastDiscovered = d
	sess := f.makeSession()
	f.lastSession = sess
	return sess, nil
}

// fakeSession implements PluginSession over an in-memory list of ApplyEvents.
type fakeSession struct {
	events   []*pluginv1.ApplyEvent
	applyErr error
	closed   bool
}

func (f *fakeSession) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &fakeApplyClient{events: f.events}, nil
}

func (f *fakeSession) Close() error {
	f.closed = true
	return nil
}

type fakeApplyClient struct {
	grpc.ClientStream
	events []*pluginv1.ApplyEvent
	idx    int
}

func (c *fakeApplyClient) Recv() (*pluginv1.ApplyEvent, error) {
	if c.idx >= len(c.events) {
		return nil, io.EOF
	}
	ev := c.events[c.idx]
	c.idx++
	return ev, nil
}

func (c *fakeApplyClient) Header() (metadata.MD, error) { return nil, nil }
func (c *fakeApplyClient) Trailer() metadata.MD         { return nil }
func (c *fakeApplyClient) CloseSend() error             { return nil }
func (c *fakeApplyClient) Context() context.Context     { return context.Background() }
func (c *fakeApplyClient) SendMsg(any) error            { return nil }
func (c *fakeApplyClient) RecvMsg(any) error {
	// apply-cycle only calls Recv(); RecvMsg stays a no-op to satisfy the
	// ClientStream interface — copying the proto message (which contains
	// sync.Mutex protoimpl.MessageState) would trip go vet.
	return nil
}

var _ = structpb.NewStruct // keep import even if unused after rebases
